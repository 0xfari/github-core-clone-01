// store.go — the forge's shared state: git object primitives (hashing,
// commit/tree/tag parse and build, reachability, pack read/write with delta
// support, pkt-lines, tree path ops, three-way diff, merge-base,
// fast-forward checks) plus the data model (repos, issues, pulls, labels).
// Every surface (git, REST, GraphQL, MCP) reads and writes this one store,
// so ids stay stable across surfaces.
//
// Determinism: no wall-clock reads, no randomness here. All timestamps come
// from MAKEFUN_TRIAL_NOW, parsed once in the Store constructor. Every
// iteration over a map that can affect output is sorted first — Go map
// iteration order is random, and the trial's state export must be
// byte-identical across runs.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Git object primitives
// ---------------------------------------------------------------------------

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

var objTypeCode = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var codeObjType = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

type GitObject struct {
	Type ObjType
	Data []byte
}

func isObjectId(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			return nil, fmt.Errorf("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			return nil, fmt.Errorf("tree entry has no name terminator")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, fmt.Errorf("tree entry has a truncated object id")
		}
		mode := string(data[offset:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		for _, c := range mode {
			if c < '0' || c > '7' {
				return nil, fmt.Errorf("tree entry mode %s is not octal", mode)
			}
		}
		entries = append(entries, TreeEntry{Mode: mode, Name: name, Oid: oid})
		offset = nul + 21
	}
	return entries, nil
}

// compareTreeNames orders entries the way git does: a directory name sorts
// as if it had a trailing slash.
func compareTreeNames(a, b TreeEntry) int {
	left := a.Name
	if a.Mode == "40000" {
		left += "/"
	}
	right := b.Name
	if b.Mode == "40000" {
		right += "/"
	}
	return bytes.Compare([]byte(left), []byte(right))
}

func buildTree(entries []TreeEntry) ([]byte, error) {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		if !isObjectId(e.Oid) {
			return nil, fmt.Errorf("tree entry %s has a bad object id", e.Name)
		}
		raw, err := hex.DecodeString(e.Oid)
		if err != nil {
			return nil, err
		}
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		buf.Write(raw)
	}
	return buf.Bytes(), nil
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) (CommitView, error) {
	var out CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headerText := string(data[:headerEnd])
	if blank >= 0 {
		out.Message = string(data[blank+2:])
	}
	for _, line := range strings.Split(headerText, "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		key, value := line, ""
		if space >= 0 {
			key, value = line[:space], line[space+1:]
		}
		switch key {
		case "tree":
			out.Tree = value
		case "parent":
			out.Parents = append(out.Parents, value)
		case "author":
			out.Author = value
		case "committer":
			out.Committer = value
		}
	}
	if !isObjectId(out.Tree) {
		return out, fmt.Errorf("commit has no valid tree header")
	}
	for _, p := range out.Parents {
		if !isObjectId(p) {
			return out, fmt.Errorf("commit has a bad parent id")
		}
	}
	return out, nil
}

type TagView struct {
	Object  string
	Type    ObjType
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) (TagView, error) {
	var out TagView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		if space < 0 {
			return out, fmt.Errorf("tag header %q has no value", line)
		}
		headers[line[:space]] = line[space+1:]
	}
	out.Object = headers["object"]
	out.Type = ObjType(headers["type"])
	if !isObjectId(out.Object) {
		return out, fmt.Errorf("tag has no valid object header")
	}
	switch out.Type {
	case ObjCommit, ObjTree, ObjBlob, ObjTag:
	default:
		return out, fmt.Errorf("tag has bad type %q", headers["type"])
	}
	out.Tag = headers["tag"]
	out.Tagger = headers["tagger"]
	if blank >= 0 {
		out.Message = string(data[blank+2:])
	}
	return out, nil
}

type BuildCommitFields struct {
	Tree      string
	Parents   []string
	Message   string
	Author    string
	Committer string
	Timestamp int64
	Timezone  string
}

func buildCommit(f BuildCommitFields) ([]byte, error) {
	if !isObjectId(f.Tree) {
		return nil, fmt.Errorf("buildCommit needs a valid tree id")
	}
	timestamp := f.Timestamp
	if timestamp == 0 {
		timestamp = 1700000000
	}
	timezone := f.Timezone
	if timezone == "" {
		timezone = "+0000"
	}
	author := f.Author
	if author == "" {
		author = "ada iversen <adaiversen@example.invalid>"
	}
	committer := f.Committer
	if committer == "" {
		committer = author
	}
	var lines []string
	lines = append(lines, "tree "+f.Tree)
	for _, p := range f.Parents {
		if !isObjectId(p) {
			return nil, fmt.Errorf("buildCommit needs valid parent ids")
		}
		lines = append(lines, "parent "+p)
	}
	lines = append(lines,
		fmt.Sprintf("author %s %d %s", author, timestamp, timezone),
		fmt.Sprintf("committer %s %d %s", committer, timestamp, timezone),
		"",
	)
	return []byte(strings.Join(lines, "\n") + "\n" + f.Message), nil
}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

func encodePktLine(payload []byte) []byte {
	if len(payload) > 65516 {
		panic("pkt-line payload too long")
	}
	header := fmt.Sprintf("%04x", len(payload)+4)
	out := make([]byte, 0, len(payload)+4)
	out = append(out, header...)
	return append(out, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// encodePktLines frames a mix of payloads, flushes and delims.
func encodePktLines(items []any) []byte {
	var out []byte
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				out = append(out, pktFlush...)
			} else if v == "delim" {
				out = append(out, pktDelim...)
			} else {
				out = append(out, encodePktLine([]byte(v))...)
			}
		case []byte:
			out = append(out, encodePktLine(v)...)
		}
	}
	return out
}

type PktKind int

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktResponseEnd
)

type PktPacket struct {
	Kind PktKind
	Text string
	Raw  []byte
}

// decodePktLines splits a buffer into pkt-line packets; throws on malformed
// framing (returned as an error here, since Go has no exceptions).
func decodePktLines(buf []byte) ([]PktPacket, error) {
	var packets []PktPacket
	offset := 0
	for offset < len(buf) {
		if offset+4 > len(buf) {
			return nil, fmt.Errorf("truncated pkt-line header")
		}
		header := string(buf[offset : offset+4])
		length, err := strconv.ParseInt(header, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("bad pkt-line length %q", header)
		}
		switch length {
		case 0:
			packets = append(packets, PktPacket{Kind: PktFlush})
			offset += 4
			continue
		case 1:
			packets = append(packets, PktPacket{Kind: PktDelim})
			offset += 4
			continue
		case 2:
			packets = append(packets, PktPacket{Kind: PktResponseEnd})
			offset += 4
			continue
		}
		if length < 4 || offset+int(length) > len(buf) {
			return nil, fmt.Errorf("truncated pkt-line packet")
		}
		raw := buf[offset+4 : offset+int(length)]
		packets = append(packets, PktPacket{Kind: PktData, Text: string(raw), Raw: raw})
		offset += int(length)
	}
	return packets, nil
}

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

// readVarintSize reads the pack entry header: type + size varint.
func readVarintSize(buf []byte, offset int) (size int, next int, first byte, err error) {
	if offset >= len(buf) {
		return 0, 0, 0, fmt.Errorf("pack header overruns buffer")
	}
	first = buf[offset]
	size = int(first & 0x0f)
	shift := uint(4)
	pos := offset + 1
	for first&0x80 != 0 {
		if pos >= len(buf) {
			return 0, 0, 0, fmt.Errorf("pack size varint overruns buffer")
		}
		b := buf[pos]
		size |= int(b&0x7f) << shift
		shift += 7
		pos++
		if b&0x80 == 0 {
			break
		}
	}
	return size, pos, first, nil
}

func readOfsDeltaBase(buf []byte, offset int) (baseOffset int, next int, err error) {
	if offset >= len(buf) {
		return 0, 0, fmt.Errorf("ofs-delta base overruns buffer")
	}
	b := buf[offset]
	pos := offset + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if pos >= len(buf) {
			return 0, 0, fmt.Errorf("ofs-delta base overruns buffer")
		}
		b = buf[pos]
		pos++
		value = ((value + 1) << 7) | int(b&0x7f)
	}
	return value, pos, nil
}

// inflateAt inflates a zlib stream starting at offset and reports the
// inflated bytes plus the offset just past the stream. The bytes.Reader is
// an io.ByteReader, so flate pulls exactly the bytes it needs and the
// remaining length tells us where the stream ended.
func inflateAt(buf []byte, offset int) (data []byte, next int, err error) {
	br := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	consumed := len(buf) - offset - br.Len()
	return out, offset + consumed, nil
}

func readDeltaVarint(delta []byte, offset int) (value int, next int, err error) {
	value = 0
	shift := uint(0)
	pos := offset
	for {
		if pos >= len(delta) {
			return 0, 0, fmt.Errorf("delta varint overruns delta")
		}
		b := delta[pos]
		pos++
		value |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	return value, pos, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	source, next, err := readDeltaVarint(delta, offset)
	if err != nil {
		return nil, err
	}
	offset = next
	target, next, err := readDeltaVarint(delta, offset)
	if err != nil {
		return nil, err
	}
	offset = next
	if source != len(base) {
		return nil, fmt.Errorf("delta source size mismatch")
	}
	var parts [][]byte
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, fmt.Errorf("delta copy overruns delta")
					}
					copyOffset |= int(delta[offset]) << (uint(bit) * 8)
					offset++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if op&(0x10<<uint(bit)) != 0 {
					if offset >= len(delta) {
						return nil, fmt.Errorf("delta copy overruns delta")
					}
					copySize |= int(delta[offset]) << (uint(bit) * 8)
					offset++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, fmt.Errorf("delta copy overruns base")
			}
			parts = append(parts, base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op > 0 {
			if offset+int(op) > len(delta) {
				return nil, fmt.Errorf("delta insert overruns delta")
			}
			parts = append(parts, delta[offset:offset+int(op)])
			offset += int(op)
			written += int(op)
		} else {
			return nil, fmt.Errorf("delta has a zero opcode")
		}
	}
	if written != target {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	return bytes.Join(parts, nil), nil
}

type PackReadResult struct {
	Objects    map[string]GitObject
	DeltaCount int
	ChecksumOK bool
}

type rawPackEntry struct {
	offset       int
	typeCode     int
	size         int
	data         []byte
	baseOffset   int
	baseOid      string
	resolvedType ObjType
	resolved     []byte
	oid          string
}

// readPack parses a packfile. Thin packs resolve REF_DELTA bases through
// resolveBase (the repo's object store); OFS_DELTA bases must be in the pack.
func readPack(pack []byte, resolveBase func(oid string) *GitObject) (PackReadResult, error) {
	var result PackReadResult
	result.Objects = map[string]GitObject{}
	if len(pack) < 32 {
		return result, fmt.Errorf("pack is too short")
	}
	if string(pack[:4]) != "PACK" {
		return result, fmt.Errorf("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return result, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	sum := sha1.Sum(pack[:trailerStart])
	result.ChecksumOK = trailer == hex.EncodeToString(sum[:])

	var raw []*rawPackEntry
	byOffset := map[int]*rawPackEntry{}
	offset := 12
	for i := 0; i < declaredCount; i++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return result, err
		}
		offset = next
		typeCode := int(first>>4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			base, next, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return result, err
			}
			offset = next
			if base <= 0 || base > entryOffset {
				return result, fmt.Errorf("ofs-delta base offset invalid")
			}
			entry.baseOffset = entryOffset - base
		} else if typeCode == 7 {
			if offset+20 > trailerStart {
				return result, fmt.Errorf("ref-delta base id is truncated")
			}
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := codeObjType[typeCode]; !ok {
			return result, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
		}
		inflated, nextOff, err := inflateAt(pack[:trailerStart], offset)
		if err != nil {
			return result, err
		}
		if len(inflated) != size {
			return result, fmt.Errorf("pack entry size mismatch after inflate")
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return result, fmt.Errorf("pack has stray bytes before its trailer")
	}

	resolvedByOid := map[string]*rawPackEntry{}
	resolving := map[int]bool{}
	var resolve func(entry *rawPackEntry) (ObjType, []byte, string, error)
	resolve = func(entry *rawPackEntry) (ObjType, []byte, string, error) {
		if entry.resolved != nil && entry.oid != "" {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle in pack")
		}
		resolving[entry.offset] = true
		defer delete(resolving, entry.offset)
		var typ ObjType
		var data []byte
		if entry.typeCode == 6 {
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				return "", nil, "", fmt.Errorf("ofs-delta base is not in the pack")
			}
			baseType, baseData, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = baseType
			data, err = applyDelta(baseData, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			if baseEntry, ok := resolvedByOid[entry.baseOid]; ok {
				baseType, baseData, _, err := resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = baseType
				data, err = applyDelta(baseData, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				if resolveBase == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor in the store", entry.baseOid)
				}
				external := resolveBase(entry.baseOid)
				if external == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor in the store", entry.baseOid)
				}
				typ = external.Type
				var err error
				data, err = applyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			typ = codeObjType[entry.typeCode]
			data = entry.data
		}
		oid := hashObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		resolvedByOid[oid] = entry
		return typ, data, oid, nil
	}

	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return result, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			result.DeltaCount++
		}
		if _, ok := result.Objects[oid]; !ok {
			result.Objects[oid] = GitObject{Type: typ, Data: data}
		}
	}
	return result, nil
}

// writePack writes a packfile with no deltas: header + zlib entries +
// SHA-1 trailer. Order is deterministic: commits, trees, blobs, tags,
// then oid — same bytes for the same object set on every run.
func writePack(objects map[string]GitObject) []byte {
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	parts := [][]byte{header}
	order := map[ObjType]int{ObjCommit: 0, ObjTree: 1, ObjBlob: 2, ObjTag: 3}
	type kv struct {
		oid string
		obj GitObject
	}
	sorted := make([]kv, 0, len(objects))
	for oid, obj := range objects {
		sorted = append(sorted, kv{oid, obj})
	}
	sort.Slice(sorted, func(i, j int) bool {
		byType := order[sorted[i].obj.Type] - order[sorted[j].obj.Type]
		if byType != 0 {
			return byType < 0
		}
		return sorted[i].oid < sorted[j].oid
	})
	for _, item := range sorted {
		typeCode := objTypeCode[item.obj.Type]
		size := len(item.obj.Data)
		var headerBytes []byte
		remaining := size >> 4
		first := byte((typeCode << 4) | (size & 0x0f))
		if remaining > 0 {
			first |= 0x80
		}
		headerBytes = append(headerBytes, first)
		for remaining > 0 {
			b := byte(remaining & 0x7f)
			remaining >>= 7
			if remaining > 0 {
				b |= 0x80
			}
			headerBytes = append(headerBytes, b)
		}
		parts = append(parts, headerBytes)
		var comp bytes.Buffer
		zw := zlib.NewWriter(&comp)
		zw.Write(item.obj.Data)
		zw.Close()
		parts = append(parts, comp.Bytes())
	}
	body := bytes.Join(parts, nil)
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

// ---------------------------------------------------------------------------
// Reachability, shallow planning, merge-base, fast-forward
// ---------------------------------------------------------------------------

type ObjectLookup func(oid string) *GitObject

func peelToCommit(lookup ObjectLookup, oid string) string {
	current := oid
	for steps := 0; steps < 10; steps++ {
		obj := lookup(current)
		if obj == nil {
			return ""
		}
		if obj.Type == ObjCommit {
			return current
		}
		if obj.Type == ObjTag {
			tag, err := parseTag(obj.Data)
			if err != nil {
				return ""
			}
			current = tag.Object
			continue
		}
		return ""
	}
	return ""
}

// collectReachable returns every object reachable from roots.
func collectReachable(lookup ObjectLookup, roots []string) map[string]GitObject {
	found := map[string]GitObject{}
	queue := append([]string{}, roots...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := found[oid]; ok {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		found[oid] = *obj
		switch obj.Type {
		case ObjCommit:
			commit, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case ObjTree:
			entries, err := parseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Mode != "160000" {
					queue = append(queue, e.Oid)
				}
			}
		case ObjTag:
			tag, err := parseTag(obj.Data)
			if err != nil {
				continue
			}
			queue = append(queue, tag.Object)
		}
	}
	return found
}

type FetchPlan struct {
	Objects   map[string]GitObject
	Shallow   []string
	Unshallow []string
	Acks      []string
}

// planFetch decides which objects to send for wants, given what the client
// already holds (haves, with shallowBefore as its shallow boundary) and a
// requested depth. Objects reachable from a have are held — except parents
// of a shallow-boundary commit, which the client does not have.
func planFetch(lookup ObjectLookup, wants, haves []string, depth *int, shallowBefore []string) FetchPlan {
	shallowSet := map[string]bool{}
	for _, o := range shallowBefore {
		shallowSet[o] = true
	}
	// Have-closure: full reachability from haves, but never cross a shallow
	// boundary commit's parents.
	haveObjects := map[string]GitObject{}
	{
		queue := append([]string{}, haves...)
		for len(queue) > 0 {
			oid := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if _, ok := haveObjects[oid]; ok {
				continue
			}
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			haveObjects[oid] = *obj
			switch obj.Type {
			case ObjCommit:
				commit, err := parseCommit(obj.Data)
				if err != nil {
					continue
				}
				queue = append(queue, commit.Tree)
				if !shallowSet[oid] {
					queue = append(queue, commit.Parents...)
				}
			case ObjTree:
				entries, err := parseTree(obj.Data)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if e.Mode != "160000" {
						queue = append(queue, e.Oid)
					}
				}
			case ObjTag:
				tag, err := parseTag(obj.Data)
				if err != nil {
					continue
				}
				queue = append(queue, tag.Object)
			}
		}
	}

	objects := map[string]GitObject{}
	includedCommits := map[string]int{}
	addObject := func(oid string) {
		if _, ok := objects[oid]; ok {
			return
		}
		if _, ok := haveObjects[oid]; ok {
			return
		}
		obj := lookup(oid)
		if obj == nil {
			return
		}
		objects[oid] = *obj
	}
	addTreeClosure := func(treeOid string) {
		queue := []string{treeOid}
		for len(queue) > 0 {
			oid := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if _, ok := objects[oid]; ok {
				continue
			}
			if _, ok := haveObjects[oid]; ok {
				continue
			}
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			objects[oid] = *obj
			if obj.Type == ObjTree {
				entries, err := parseTree(obj.Data)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if e.Mode != "160000" {
						queue = append(queue, e.Oid)
					}
				}
			}
		}
	}

	type qitem struct {
		oid      string
		distance int
	}
	var queue []qitem
	for _, want := range wants {
		obj := lookup(want)
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			addObject(want)
			target := peelToCommit(lookup, want)
			if target != "" {
				queue = append(queue, qitem{target, 0})
			} else {
				tag, err := parseTag(obj.Data)
				if err == nil {
					addObject(tag.Object)
				}
			}
		} else if obj.Type == ObjCommit {
			queue = append(queue, qitem{want, 0})
		} else {
			addObject(want)
		}
	}
	seenCommit := map[string]bool{}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if seenCommit[item.oid] {
			continue
		}
		seenCommit[item.oid] = true
		obj := lookup(item.oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		include := (depth == nil || item.distance < *depth)
		if _, held := haveObjects[item.oid]; held {
			include = false
		}
		if include {
			objects[item.oid] = *obj
			includedCommits[item.oid] = item.distance
			commit, err := parseCommit(obj.Data)
			if err == nil {
				addTreeClosure(commit.Tree)
			}
		}
		// Parents past the depth cut can never be included.
		if depth != nil && item.distance+1 >= *depth {
			continue
		}
		commit, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, parent := range commit.Parents {
			if !seenCommit[parent] {
				queue = append(queue, qitem{parent, item.distance + 1})
			}
		}
	}

	var shallow []string
	if depth != nil {
		for oid, distance := range includedCommits {
			if distance != *depth-1 {
				continue
			}
			obj := lookup(oid)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			commit, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			if len(commit.Parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
		sort.Strings(shallow)
	}
	var unshallow []string
	for _, oid := range shallowBefore {
		if _, ok := includedCommits[oid]; ok {
			unshallow = append(unshallow, oid)
		}
	}

	var acks []string
	if len(haves) > 0 {
		wantClosure := collectReachable(lookup, wants)
		for _, have := range haves {
			if _, ok := wantClosure[have]; ok {
				acks = append(acks, have)
			}
		}
	}
	return FetchPlan{Objects: objects, Shallow: shallow, Unshallow: unshallow, Acks: acks}
}

// mergeBase returns the nearest common ancestor of two commits.
func mergeBase(lookup ObjectLookup, a, b string) string {
	aAncestors := map[string]bool{}
	queueA := []string{a}
	for len(queueA) > 0 {
		oid := queueA[len(queueA)-1]
		queueA = queueA[:len(queueA)-1]
		if aAncestors[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		aAncestors[oid] = true
		commit, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		queueA = append(queueA, commit.Parents...)
	}
	queueB := []string{b}
	seenB := map[string]bool{}
	for len(queueB) > 0 {
		oid := queueB[0]
		queueB = queueB[1:]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if aAncestors[oid] {
			return oid
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		commit, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		queueB = append(queueB, commit.Parents...)
	}
	return ""
}

// isAncestor reports whether ancestor is reachable from descendant (or equal).
func isAncestor(lookup ObjectLookup, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	queue := []string{descendant}
	seen := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		commit, err := parseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, parent := range commit.Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

type CommitEntry struct {
	Sha    string
	Commit CommitView
}

// listCommits returns commits reachable from a tip, newest-first (DFS pre-order).
func listCommits(lookup ObjectLookup, tip string) []CommitEntry {
	var out []CommitEntry
	seen := map[string]bool{}
	var walk func(oid string)
	walk = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjCommit {
			return
		}
		commit, err := parseCommit(obj.Data)
		if err != nil {
			return
		}
		out = append(out, CommitEntry{Sha: oid, Commit: commit})
		for _, parent := range commit.Parents {
			walk(parent)
		}
	}
	walk(tip)
	return out
}

// ---------------------------------------------------------------------------
// Tree path operations (REST contents)
// ---------------------------------------------------------------------------

type PathEntry struct {
	Type string // "blob" or "tree"
	Oid  string
}

// readPath reads a path inside a tree: file blob or subtree.
func readPath(lookup ObjectLookup, treeOid string, segments []string) *PathEntry {
	current := treeOid
	for i, seg := range segments {
		obj := lookup(current)
		if obj == nil || obj.Type != ObjTree {
			return nil
		}
		entries, err := parseTree(obj.Data)
		if err != nil {
			return nil
		}
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(segments)-1 {
			if found.Mode == "40000" {
				return &PathEntry{Type: "tree", Oid: found.Oid}
			}
			return &PathEntry{Type: "blob", Oid: found.Oid}
		}
		if found.Mode != "40000" {
			return nil
		}
		current = found.Oid
	}
	return nil
}

// setPath sets (or deletes, when blobOid is "") a file at segments inside
// the tree rooted at treeOid; stores new trees through put and returns the
// new root tree oid. Intermediate trees are created as needed; empty
// directories are pruned on delete.
func setPath(lookup ObjectLookup, put func(typ ObjType, data []byte) string, treeOid string, segments []string, blobOid string) (string, error) {
	head := segments[0]
	rest := segments[1:]
	var entries []TreeEntry
	if treeOid != "" {
		obj := lookup(treeOid)
		if obj != nil {
			parsed, err := parseTree(obj.Data)
			if err != nil {
				return "", err
			}
			entries = parsed
		}
	}
	var existing *TreeEntry
	for i := range entries {
		if entries[i].Name == head {
			existing = &entries[i]
			break
		}
	}
	var newEntries []TreeEntry
	for _, e := range entries {
		if e.Name != head {
			newEntries = append(newEntries, e)
		}
	}
	if len(rest) == 0 {
		if blobOid != "" {
			newEntries = append(newEntries, TreeEntry{Mode: "100644", Name: head, Oid: blobOid})
		}
	} else {
		var childTree string
		if existing != nil && existing.Mode == "40000" {
			childTree = existing.Oid
		}
		newChild, err := setPath(lookup, put, childTree, rest, blobOid)
		if err != nil {
			return "", err
		}
		if blobOid == "" {
			childObj := lookup(newChild)
			childEntries := []TreeEntry{}
			if childObj != nil {
				parsed, err := parseTree(childObj.Data)
				if err == nil {
					childEntries = parsed
				}
			}
			if len(childEntries) > 0 {
				newEntries = append(newEntries, TreeEntry{Mode: "40000", Name: head, Oid: newChild})
			}
		} else {
			newEntries = append(newEntries, TreeEntry{Mode: "40000", Name: head, Oid: newChild})
		}
	}
	data, err := buildTree(newEntries)
	if err != nil {
		return "", err
	}
	return put(ObjTree, data), nil
}

// flattenTree flattens a tree to path -> {mode, oid} for every file.
func flattenTree(lookup ObjectLookup, treeOid, prefix string) map[string]TreeEntry {
	out := map[string]TreeEntry{}
	obj := lookup(treeOid)
	if obj == nil || obj.Type != ObjTree {
		return out
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return out
	}
	for _, e := range entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			for sub, v := range flattenTree(lookup, e.Oid, path) {
				out[sub] = v
			}
		} else {
			out[path] = e
		}
	}
	return out
}

type FileChange struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
}

func countLines(buf []byte) []string {
	text := string(buf)
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiffCounts computes line diff counts via LCS (files are small).
func lineDiffCounts(oldBuf, newBuf []byte) (additions, deletions int) {
	oldLines := countLines(oldBuf)
	newLines := countLines(newBuf)
	n, m := len(oldLines), len(newLines)
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if oldLines[i-1] == newLines[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[m]
	return m - lcs, n - lcs
}

// diffTrees diffs two trees (merge-base -> head) as a pull-request file list.
func diffTrees(lookup ObjectLookup, baseTreeOid, headTreeOid string) []FileChange {
	base := map[string]TreeEntry{}
	if baseTreeOid != "" {
		base = flattenTree(lookup, baseTreeOid, "")
	}
	head := flattenTree(lookup, headTreeOid, "")
	pathSet := map[string]bool{}
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []FileChange
	for _, path := range paths {
		b, hasB := base[path]
		h, hasH := head[path]
		switch {
		case !hasB && hasH:
			data := []byte{}
			if obj := lookup(h.Oid); obj != nil {
				data = obj.Data
			}
			add := len(countLines(data))
			out = append(out, FileChange{Filename: path, Status: "added", Additions: add, Deletions: 0, Changes: add})
		case hasB && !hasH:
			data := []byte{}
			if obj := lookup(b.Oid); obj != nil {
				data = obj.Data
			}
			del := len(countLines(data))
			out = append(out, FileChange{Filename: path, Status: "removed", Additions: 0, Deletions: del, Changes: del})
		case hasB && hasH && b.Oid != h.Oid:
			oldData := []byte{}
			if obj := lookup(b.Oid); obj != nil {
				oldData = obj.Data
			}
			newData := []byte{}
			if obj := lookup(h.Oid); obj != nil {
				newData = obj.Data
			}
			add, del := lineDiffCounts(oldData, newData)
			out = append(out, FileChange{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	return out
}

// mergeConflicts reports whether a three-way merge of headTree onto baseTree
// (both descended from mergeBaseTree) would conflict.
func mergeConflicts(lookup ObjectLookup, mergeBaseTree, headTree, baseTree string) bool {
	base := map[string]TreeEntry{}
	if mergeBaseTree != "" {
		base = flattenTree(lookup, mergeBaseTree, "")
	}
	head := flattenTree(lookup, headTree, "")
	current := flattenTree(lookup, baseTree, "")
	pathSet := map[string]bool{}
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	for p := range current {
		pathSet[p] = true
	}
	for path := range pathSet {
		b := ""
		if e, ok := base[path]; ok {
			b = e.Oid
		}
		h := ""
		if e, ok := head[path]; ok {
			h = e.Oid
		}
		c := ""
		if e, ok := current[path]; ok {
			c = e.Oid
		}
		if h != b && c != b && h != c {
			return true
		}
	}
	return false
}

// applyMerge applies the head side's changes (vs merge-base) onto the
// current base tree.
func applyMerge(lookup ObjectLookup, put func(typ ObjType, data []byte) string, mergeBaseTree, headTree, baseTree string) (string, error) {
	base := map[string]TreeEntry{}
	if mergeBaseTree != "" {
		base = flattenTree(lookup, mergeBaseTree, "")
	}
	head := flattenTree(lookup, headTree, "")
	pathSet := map[string]bool{}
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	root := baseTree
	var err error
	for path := range pathSet {
		b := ""
		if e, ok := base[path]; ok {
			b = e.Oid
		}
		h := ""
		if e, ok := head[path]; ok {
			h = e.Oid
		}
		if h != b {
			root, err = setPath(lookup, put, root, strings.Split(path, "/"), h)
			if err != nil {
				return "", err
			}
		}
	}
	return root, nil
}

// ---------------------------------------------------------------------------
// The data model
// ---------------------------------------------------------------------------

type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type CommentItem struct {
	ID        int
	Body      string
	UserLogin string
	CreatedAt string
}

type IssueItem struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []*Label
	Comments  []*CommentItem
	UserLogin string
	Seq       int
	// Mutation counter; bumped on every change to this issue.
	Rev int
	// Serialized issue JSON, valid only while Rev matches; empty when stale.
	JSONCache string
	// Repo back-pointer for GraphQL global ids; never serialized.
	repo *Repo
}

type PullItem struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string // "" when null
	MergeCommitSha string // "" when null
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	MergeBaseSha   string // "" when null
	Files          []FileChange
	Seq            int
	// Repo back-pointer for GraphQL; never serialized.
	repo *Repo
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	IsPrivate     bool
	DefaultBranch string
	Objects       map[string]GitObject
	Refs          map[string]string
	Labels        []*Label
	Issues        []*IssueItem
	Pulls         []*PullItem
	NextNumber    int
}

const ZeroOid = "0000000000000000000000000000000000000000"

type Store struct {
	// Repos in creation order (insertion order, like a JS Map). The lookup
	// map is only for O(1) access; iteration always uses ReposOrdered.
	ReposOrdered  []*Repo
	ReposByKey    map[string]*Repo
	NextRepoID    int
	NextIssueID   int
	NextCommentID int
	NextLabelID   int
	RequestSeq    int
	// Bounded cache of computed git response bytes. Keyed by repo id + a
	// fingerprint of refs + object count + request args. Sound because
	// objects are append-only and content-addressed, and refs are part of
	// the fingerprint; a deleted repo gets a fresh id when recreated.
	GitCache map[string][]byte

	TrialNowIso   string
	TrialNowEpoch int64
	Token         string

	ViewerLogin string
	ViewerID    int
	ViewerName  string
	ViewerEmail string
}

func NewStore() *Store {
	s := &Store{
		ReposByKey:    map[string]*Repo{},
		NextRepoID:    1,
		NextIssueID:   1,
		NextCommentID: 1,
		NextLabelID:   1,
		GitCache:      map[string][]byte{},
		ViewerLogin:   "adaiversen",
		ViewerID:      1000,
		ViewerName:    "ada iversen",
		ViewerEmail:   "adaiversen@example.invalid",
	}
	rawNow := envStr("MAKEFUN_TRIAL_NOW", "")
	if rawNow != "" {
		if epoch, iso, ok := parseTrialNow(rawNow); ok {
			s.TrialNowIso = iso
			s.TrialNowEpoch = epoch
			s.Token = envStr("MAKEFUN_SHOP_TOKEN", "")
			return s
		}
	}
	s.TrialNowIso = "2026-11-14T00:00:00.000Z"
	s.TrialNowEpoch = 1792531200
	s.Token = envStr("MAKEFUN_SHOP_TOKEN", "")
	return s
}

func (s *Store) GitCacheGet(key string) ([]byte, bool) {
	v, ok := s.GitCache[key]
	return v, ok
}

func (s *Store) GitCacheSet(key string, buf []byte) {
	if len(buf) > 65536 {
		return
	}
	if _, ok := s.GitCache[key]; !ok && len(s.GitCache) >= 64 {
		// Evict the oldest entry (map order is random; eviction choice
		// does not affect output, only cache hit rate).
		for k := range s.GitCache {
			delete(s.GitCache, k)
			break
		}
	}
	s.GitCache[key] = buf
}

func (s *Store) NextRequestID() string {
	s.RequestSeq++
	return "req-" + strconv.FormatInt(int64(s.RequestSeq), 16)
}

func (s *Store) RepoKey(owner, name string) string { return owner + "/" + name }

// getRepo: repos only ever exist under the viewer; any other owner is a 404.
func (s *Store) GetRepo(owner, name string) *Repo {
	if owner != s.ViewerLogin {
		return nil
	}
	return s.ReposByKey[s.RepoKey(owner, name)]
}

func (s *Store) CreateRepo(name, description string, isPrivate bool) *Repo {
	repo := &Repo{
		ID:            s.NextRepoID,
		Owner:         s.ViewerLogin,
		Name:          name,
		Description:   description,
		IsPrivate:     isPrivate,
		DefaultBranch: "main",
		Objects:       map[string]GitObject{},
		Refs:          map[string]string{},
		NextNumber:    1,
	}
	s.NextRepoID++
	s.ReposOrdered = append(s.ReposOrdered, repo)
	s.ReposByKey[s.RepoKey(s.ViewerLogin, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	key := s.RepoKey(owner, name)
	if _, ok := s.ReposByKey[key]; !ok {
		return false
	}
	delete(s.ReposByKey, key)
	for i, r := range s.ReposOrdered {
		if r.Owner == owner && r.Name == name {
			s.ReposOrdered = append(s.ReposOrdered[:i], s.ReposOrdered[i+1:]...)
			break
		}
	}
	return true
}

func (s *Store) ListViewerRepos() []*Repo { return s.ReposOrdered }

func (s *Store) PutObject(repo *Repo, typ ObjType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = GitObject{Type: typ, Data: data}
	}
	return oid
}

func (s *Store) Lookup(repo *Repo) ObjectLookup {
	return func(oid string) *GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			o := obj
			return &o
		}
		return nil
	}
}

// Identity is the identity line every server-built commit carries.
func (s *Store) Identity() string { return s.ViewerName + " <" + s.ViewerEmail + ">" }

// MakeCommit builds + stores a commit; returns its oid.
func (s *Store) MakeCommit(repo *Repo, f BuildCommitFields) string {
	if f.Author == "" {
		f.Author = s.Identity()
	}
	if f.Committer == "" {
		f.Committer = s.Identity()
	}
	if f.Timestamp == 0 {
		f.Timestamp = s.TrialNowEpoch
	}
	data, err := buildCommit(f)
	if err != nil {
		return ""
	}
	return s.PutObject(repo, ObjCommit, data)
}

// NextRepoNumber creates an issue or PR number (one shared sequence per repo).
func (s *Store) NextRepoNumber(repo *Repo) int {
	n := repo.NextNumber
	repo.NextNumber++
	return n
}

// Global id helpers (base64 of a typed string).
func (s *Store) RepoGlobalID(repo *Repo) string {
	return b64enc("R:" + strconv.Itoa(repo.ID))
}

func (s *Store) IssueGlobalID(repo *Repo, number int) string {
	return b64enc("I:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(number))
}

func (s *Store) PullGlobalID(repo *Repo, number int) string {
	return b64enc("PR:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(number))
}

func (s *Store) CommentGlobalID(repo *Repo, issueNumber, commentID int) string {
	return b64enc("IC:" + strconv.Itoa(repo.ID) + ":" + strconv.Itoa(issueNumber) + ":" + strconv.Itoa(commentID))
}

// DecodeGlobalID decodes a global id into its typed parts, or nil when malformed.
func (s *Store) DecodeGlobalID(id string) (kind string, parts []string, ok bool) {
	decoded, err := b64dec(id)
	if err != nil {
		return "", nil, false
	}
	p := strings.Split(decoded, ":")
	if len(p) < 2 {
		return "", nil, false
	}
	return p[0], p[1:], true
}

func (s *Store) FindRepoByDbID(dbID int) *Repo {
	for _, repo := range s.ReposOrdered {
		if repo.ID == dbID {
			return repo
		}
	}
	return nil
}

// RepoFromGlobalID resolves a repo global id to its repo, or nil.
func (s *Store) RepoFromGlobalID(id string) *Repo {
	kind, parts, ok := s.DecodeGlobalID(id)
	if !ok || kind != "R" || len(parts) != 1 {
		return nil
	}
	dbID, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil
	}
	return s.FindRepoByDbID(dbID)
}

// ---------------------------------------------------------------------------
// Shared mutation internals (used by GraphQL and MCP; the REST handlers
// build the same records inline, exactly as the oracle does).
// ---------------------------------------------------------------------------

// CreateIssue appends an issue to a repo. Mirrors createIssueInternal:
// the numeric id and the sequence number both come from the same counter.
func (s *Store) CreateIssue(repo *Repo, title, body string) *IssueItem {
	issue := &IssueItem{
		ID:        s.NextIssueID,
		Number:    s.NextRepoNumber(repo),
		Title:     title,
		Body:      body,
		State:     "open",
		UserLogin: s.ViewerLogin,
		Seq:       s.NextIssueID,
	}
	issue.repo = repo
	s.NextIssueID++
	repo.Issues = append(repo.Issues, issue)
	return issue
}

// CreatePullInternal creates a pull request from a head branch into a base
// branch. The body is always empty (the oracle's createPullInternal takes
// no body). Returns nil when either branch tip is missing or an open pull
// request already exists for the same head/base pair.
func (s *Store) CreatePullInternal(repo *Repo, title, head, base string) *PullItem {
	headTip := repo.Refs["refs/heads/"+head]
	baseTip := repo.Refs["refs/heads/"+base]
	if headTip == "" || baseTip == "" {
		return nil
	}
	for _, p := range repo.Pulls {
		if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
			return nil
		}
	}
	lookup := s.Lookup(repo)
	mb := mergeBase(lookup, headTip, baseTip)
	var headTree string
	if headObj := lookup(headTip); headObj != nil {
		if c, err := parseCommit(headObj.Data); err == nil {
			headTree = c.Tree
		}
	}
	var mbTree string
	if mb != "" {
		if mbObj := lookup(mb); mbObj != nil {
			if c, err := parseCommit(mbObj.Data); err == nil {
				mbTree = c.Tree
			}
		}
	}
	pr := &PullItem{
		ID:           s.NextIssueID,
		Number:       s.NextRepoNumber(repo),
		Title:        title,
		Body:         "",
		State:        "open",
		HeadRef:      head,
		BaseRef:      base,
		HeadSha:      headTip,
		BaseSha:      baseTip,
		MergeBaseSha: mb,
		Files:        diffTrees(lookup, mbTree, headTree),
		Seq:          s.NextIssueID,
	}
	pr.repo = repo
	s.NextIssueID++
	repo.Pulls = append(repo.Pulls, pr)
	return pr
}

// MergePullInternal squash-merges an open pull request onto its base branch
// with a merge commit, and returns the merge commit's sha. Returns "" when
// the base tip is missing or the merge conflicts.
func (s *Store) MergePullInternal(repo *Repo, pr *PullItem) string {
	lookup := s.Lookup(repo)
	baseTip := repo.Refs["refs/heads/"+pr.BaseRef]
	if baseTip == "" {
		return ""
	}
	baseObj := lookup(baseTip)
	headObj := lookup(pr.HeadSha)
	if baseObj == nil || headObj == nil {
		return ""
	}
	baseCommit, err1 := parseCommit(baseObj.Data)
	headCommit, err2 := parseCommit(headObj.Data)
	if err1 != nil || err2 != nil {
		return ""
	}
	mb := mergeBase(lookup, pr.HeadSha, baseTip)
	var mbTree string
	if mb != "" {
		if mbObj := lookup(mb); mbObj != nil {
			if c, err := parseCommit(mbObj.Data); err == nil {
				mbTree = c.Tree
			}
		}
	}
	if mergeConflicts(lookup, mbTree, headCommit.Tree, baseCommit.Tree) {
		return ""
	}
	mergedTree, err := applyMerge(lookup, func(typ ObjType, data []byte) string {
		return s.PutObject(repo, typ, data)
	}, mbTree, headCommit.Tree, baseCommit.Tree)
	if err != nil {
		return ""
	}
	sha := s.MakeCommit(repo, BuildCommitFields{
		Tree:    mergedTree,
		Parents: []string{baseTip},
		Message: "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + repo.Owner + "/" + pr.HeadRef + "\n\n" + pr.Title,
	})
	repo.Refs["refs/heads/"+pr.BaseRef] = sha
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = s.TrialNowIso
	pr.MergeCommitSha = sha
	return sha
}
