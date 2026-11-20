// gitproto.go — git smart HTTP: info/refs advertisements, upload-pack
// (protocol v2 and v0), and receive-pack (v0). All object work goes through
// store.go; this module only frames the protocol.
//
// Git routes carry no auth check (public clone semantics); a repo that does
// not exist — or lives under another owner — is a 404 on every service.

package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	uploadCT        = "application/x-git-upload-pack-advertisement"
	uploadResultCT  = "application/x-git-upload-pack-result"
	receiveCT       = "application/x-git-receive-pack-advertisement"
	receiveResultCT = "application/x-git-receive-pack-result"
)

// The v2 capability advertisement never depends on the repo: one buffer.
var v2Advertisement = encodePktLines([]any{
	"version 2\n",
	"ls-refs=unborn\n",
	"fetch=shallow filter\n",
	"object-format=sha1\n",
	"agent=forge/1.0.0\n",
	"flush",
})

func repoFromPath(ctx *Ctx) (*Repo, error) {
	owner := ""
	if len(ctx.Segments) > 0 {
		owner = ctx.Segments[0]
	}
	repoName := ""
	if len(ctx.Segments) > 1 {
		repoName = strings.TrimSuffix(ctx.Segments[1], ".git")
	}
	repo := ctx.Store.GetRepo(owner, repoName)
	if repo == nil {
		return nil, apiErr(404, "Not Found")
	}
	return repo, nil
}

// refsFingerprint is the repo state a git response depends on: every ref tip
// (sorted) plus the object count. Sound as a cache key because objects are
// append-only and content-addressed, and a recreated repo gets a fresh id.
func refsFingerprint(repo *Repo) string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		parts = append(parts, name+"="+repo.Refs[name])
	}
	return strings.Join(parts, ",")
}

func gitCacheKey(repo *Repo, tag, args string) string {
	return fmt.Sprintf("%d|%s|%s|%d|%s", repo.ID, tag, refsFingerprint(repo), len(repo.Objects), args)
}

func defaultTip(repo *Repo) string {
	return repo.Refs["refs/heads/"+repo.DefaultBranch]
}

func sortedRefs(repo *Repo) []struct {
	Name string
	Oid  string
} {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]struct {
		Name string
		Oid  string
	}, 0, len(names))
	for _, name := range names {
		out = append(out, struct {
			Name string
			Oid  string
		}{Name: name, Oid: repo.Refs[name]})
	}
	return out
}

// v0Advertisement builds a v0 advertisement for a service.
func v0Advertisement(repo *Repo, service, caps string) []byte {
	var lines []any
	lines = append(lines, "# service="+service+"\n", "flush")
	refs := sortedRefs(repo)
	tip := defaultTip(repo)
	if len(refs) == 0 || tip == "" {
		lines = append(lines, ZeroOid+" capabilities^{}\x00"+caps+"\n")
	} else {
		lines = append(lines, tip+" HEAD\x00"+caps+"\n")
		for _, ref := range refs {
			lines = append(lines, ref.Oid+" "+ref.Name+"\n")
			// Peeled line for an annotated tag: the tag's target commit.
			if obj, ok := repo.Objects[ref.Oid]; ok && obj.Type == ObjTag && strings.HasPrefix(ref.Name, "refs/tags/") {
				if target, err := parseTag(obj.Data); err == nil {
					if targetObj, ok := repo.Objects[target.Object]; ok && targetObj.Type == ObjCommit {
						lines = append(lines, target.Object+" "+ref.Name+"^{}\n")
					}
				}
			}
		}
	}
	lines = append(lines, "flush")
	return encodePktLines(lines)
}

const uploadV0Caps = "symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta shallow multi_ack multi_ack_detailed agent=forge/1.0.0"
const receiveV0Caps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0.0"

// handleInfoRefs serves GET /{owner}/{repo}.git/info/refs?service=...
func handleInfoRefs(ctx *Ctx, w http.ResponseWriter) {
	repo, err := repoFromPath(ctx)
	if err != nil {
		panic(err)
	}
	service := ctx.Query.Get("service")
	v2 := strings.Contains(ctx.Headers.Get("Git-Protocol"), "version=2")
	if service == "git-upload-pack" {
		if v2 {
			sendBuffer(w, 200, v2Advertisement, uploadCT)
			return
		}
		key := gitCacheKey(repo, "adv-upload-v0", "")
		if cached, ok := ctx.Store.GitCacheGet(key); ok {
			sendBuffer(w, 200, cached, uploadCT)
			return
		}
		body := v0Advertisement(repo, "git-upload-pack", uploadV0Caps)
		ctx.Store.GitCacheSet(key, body)
		sendBuffer(w, 200, body, uploadCT)
		return
	}
	if service == "git-receive-pack" {
		key := gitCacheKey(repo, "adv-receive-v0", "")
		if cached, ok := ctx.Store.GitCacheGet(key); ok {
			sendBuffer(w, 200, cached, receiveCT)
			return
		}
		body := v0Advertisement(repo, "git-receive-pack", receiveV0Caps)
		ctx.Store.GitCacheSet(key, body)
		sendBuffer(w, 200, body, receiveCT)
		return
	}
	panic(apiErr(400, "unknown service"))
}

// ---------------------------------------------------------------------------
// upload-pack v2
// ---------------------------------------------------------------------------

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func handleLsRefs(store *Store, repo *Repo, args lsRefsArgs) []byte {
	key := gitCacheKey(repo, "ls-refs", fmt.Sprintf("%v|%v|%v|%v", args.peel, args.symrefs, args.unborn, args.prefixes))
	if cached, ok := store.GitCacheGet(key); ok {
		return cached
	}
	var lines []string
	tip := defaultTip(repo)
	include := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, p := range args.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	refs := sortedRefs(repo)
	if tip == "" {
		if args.unborn {
			line := "unborn HEAD"
			if args.symrefs {
				line += " symref-target:refs/heads/main"
			}
			lines = append(lines, line+"\n")
		}
	} else {
		if include("HEAD") {
			line := tip + " HEAD"
			if args.symrefs {
				line += " symref-target:refs/heads/main"
			}
			lines = append(lines, line+"\n")
		}
		for _, ref := range refs {
			if !include(ref.Name) {
				continue
			}
			line := ref.Oid + " " + ref.Name
			if obj, ok := repo.Objects[ref.Oid]; ok && args.peel && obj.Type == ObjTag {
				if target, err := parseTag(obj.Data); err == nil {
					if targetObj, ok := repo.Objects[target.Object]; ok && targetObj.Type == ObjCommit {
						line += " peeled:" + target.Object
					}
				}
			}
			lines = append(lines, line+"\n")
		}
	}
	var items []any
	for _, l := range lines {
		items = append(items, l)
	}
	items = append(items, "flush")
	body := encodePktLines(items)
	store.GitCacheSet(key, body)
	return body
}

type fetchArgs struct {
	wants      []string
	haves      []string
	shallows   []string
	depth      *int
	thinPack   bool
	ofsDelta   bool
	noProgress bool
}

func parseV2Args(packets []PktPacket) []string {
	var args []string
	for _, p := range packets {
		if p.Kind == PktData {
			args = append(args, p.Text)
		}
	}
	return args
}

// sidebandPackets splits sideband band-1 data into pkt-lines (band byte +
// up to 65515 bytes).
func sidebandPackets(data []byte, band byte) [][]byte {
	var out [][]byte
	const chunkSize = 65515
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		framed := append([]byte{band}, data[offset:end]...)
		out = append(out, encodePktLine(framed))
	}
	return out
}

func handleV2Fetch(store *Store, repo *Repo, args fetchArgs, rawArgs []string) []byte {
	// The whole response is a pure function of the repo state + the
	// request's args (the pack is built deterministically), so identical
	// fetches — the 50 concurrent depth-1 clones of one tip — are served
	// from one cached buffer instead of rebuilding the pack each time.
	key := gitCacheKey(repo, "fetch-v2", fmt.Sprintf("%q", rawArgs))
	if cached, ok := store.GitCacheGet(key); ok {
		return cached
	}
	lookup := store.Lookup(repo)
	var unknownWant string
	for _, w := range args.wants {
		if _, ok := repo.Objects[w]; !ok {
			unknownWant = w
			break
		}
	}
	plan := planFetch(lookup, args.wants, args.haves, args.depth, args.shallows)
	var sections [][][]byte
	// The acknowledgments section exists only when the client sent haves;
	// a fresh clone (no haves) expects the response to open with
	// shallow-info or packfile.
	if len(args.haves) > 0 {
		ackLines := [][]byte{[]byte("acknowledgments\n")}
		if len(plan.Acks) > 0 {
			for _, ack := range plan.Acks {
				ackLines = append(ackLines, []byte("ACK "+ack+"\n"))
			}
			ackLines = append(ackLines, []byte("ready\n"))
		} else {
			ackLines = append(ackLines, []byte("NAK\n"))
		}
		var framed [][]byte
		for _, line := range ackLines {
			framed = append(framed, encodePktLine(line))
		}
		sections = append(sections, framed)
	}
	if args.depth != nil || len(args.shallows) > 0 {
		info := [][]byte{[]byte("shallow-info\n")}
		for _, oid := range plan.Shallow {
			info = append(info, []byte("shallow "+oid+"\n"))
		}
		for _, oid := range plan.Unshallow {
			info = append(info, []byte("unshallow "+oid+"\n"))
		}
		var framed [][]byte
		for _, line := range info {
			framed = append(framed, encodePktLine(line))
		}
		sections = append(sections, framed)
	}
	if unknownWant != "" {
		errPkt := append([]byte{3}, []byte("error: object "+unknownWant+" not found\n")...)
		sections = append(sections, [][]byte{encodePktLine([]byte("packfile\n")), encodePktLine(errPkt)})
	} else if len(plan.Objects) > 0 {
		pack := writePack(plan.Objects)
		packSection := [][]byte{encodePktLine([]byte("packfile\n"))}
		packSection = append(packSection, sidebandPackets(pack, 1)...)
		sections = append(sections, packSection)
	}
	var parts [][]byte
	for i, sec := range sections {
		parts = append(parts, bytes.Join(sec, nil))
		if i == len(sections)-1 {
			parts = append(parts, pktFlush)
		} else {
			parts = append(parts, pktDelim)
		}
	}
	body := bytes.Join(parts, nil)
	store.GitCacheSet(key, body)
	return body
}

func handleV2UploadPack(store *Store, repo *Repo, packets []PktPacket) ([]byte, error) {
	var first *PktPacket
	for i := range packets {
		if packets[i].Kind == PktData {
			first = &packets[i]
			break
		}
	}
	if first == nil {
		return nil, apiErr(400, "empty upload-pack request")
	}
	commandLine := strings.TrimSpace(first.Text)
	if !strings.HasPrefix(commandLine, "command=") {
		return nil, apiErr(400, "upload-pack request has no command")
	}
	command := strings.TrimSpace(commandLine[len("command="):])
	args := parseV2Args(packets[1:])
	if command == "ls-refs" {
		parsed := lsRefsArgs{}
		for _, arg := range args {
			text := strings.TrimSpace(arg)
			switch {
			case text == "peel":
				parsed.peel = true
			case text == "symrefs":
				parsed.symrefs = true
			case text == "unborn":
				parsed.unborn = true
			case strings.HasPrefix(text, "ref-prefix "):
				parsed.prefixes = append(parsed.prefixes, text[len("ref-prefix "):])
			}
		}
		return handleLsRefs(store, repo, parsed), nil
	}
	if command == "fetch" {
		parsed := fetchArgs{}
		for _, arg := range args {
			text := strings.TrimSpace(arg)
			switch {
			case strings.HasPrefix(text, "want "):
				parsed.wants = append(parsed.wants, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "have "):
				parsed.haves = append(parsed.haves, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "shallow "):
				parsed.shallows = append(parsed.shallows, strings.TrimSpace(text[8:]))
			case strings.HasPrefix(text, "deepen "):
				n := 0
				fmt.Sscanf(strings.TrimSpace(text[7:]), "%d", &n)
				parsed.depth = &n
			case text == "thin-pack":
				parsed.thinPack = true
			case text == "ofs-delta":
				parsed.ofsDelta = true
			case text == "no-progress":
				parsed.noProgress = true
			}
		}
		return handleV2Fetch(store, repo, parsed, args), nil
	}
	return nil, apiErr(400, "unknown upload-pack command "+command)
}

// ---------------------------------------------------------------------------
// upload-pack v0 (fallback fetch)
// ---------------------------------------------------------------------------

func handleV0UploadPack(store *Store, repo *Repo, packets []PktPacket) []byte {
	args := fetchArgs{}
	sideband := false
	var rawArgs []string
	for _, packet := range packets {
		if packet.Kind != PktData {
			continue
		}
		text := packet.Text
		rawArgs = append(rawArgs, text)
		if nul := strings.Index(text, "\x00"); nul >= 0 {
			caps := text[nul+1:]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
			text = text[:nul]
		}
		text = strings.TrimSpace(text)
		switch {
		case strings.HasPrefix(text, "want "):
			args.wants = append(args.wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.shallows = append(args.shallows, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			n := 0
			fmt.Sscanf(strings.TrimSpace(text[7:]), "%d", &n)
			args.depth = &n
		}
	}
	tag := "fetch-v0"
	if sideband {
		tag = "fetch-v0-sb"
	}
	key := gitCacheKey(repo, tag, fmt.Sprintf("%q", rawArgs))
	if cached, ok := store.GitCacheGet(key); ok {
		return cached
	}
	lookup := store.Lookup(repo)
	plan := planFetch(lookup, args.wants, args.haves, args.depth, args.shallows)
	var parts [][]byte
	if len(plan.Acks) > 0 {
		for _, ack := range plan.Acks {
			parts = append(parts, encodePktLine([]byte("ACK "+ack+"\n")))
		}
		parts = append(parts, encodePktLine([]byte("ready\n")))
	} else {
		parts = append(parts, encodePktLine([]byte("NAK\n")))
	}
	for _, oid := range plan.Shallow {
		parts = append(parts, encodePktLine([]byte("shallow "+oid+"\n")))
	}
	for _, oid := range plan.Unshallow {
		parts = append(parts, encodePktLine([]byte("unshallow "+oid+"\n")))
	}
	if len(plan.Objects) > 0 {
		pack := writePack(plan.Objects)
		if sideband {
			parts = append(parts, sidebandPackets(pack, 1)...)
		} else {
			parts = append(parts, pack)
		}
	}
	parts = append(parts, pktFlush)
	body := bytes.Join(parts, nil)
	store.GitCacheSet(key, body)
	return body
}

// handleUploadPack serves POST /{owner}/{repo}.git/git-upload-pack.
func handleUploadPack(ctx *Ctx, w http.ResponseWriter) {
	repo, err := repoFromPath(ctx)
	if err != nil {
		panic(err)
	}
	packets, err := decodePktLines(ctx.Body)
	if err != nil {
		panic(apiErr(400, err.Error()))
	}
	isV2 := false
	if len(ctx.Body) >= 4 {
		for _, p := range packets {
			if p.Kind == PktData {
				if strings.HasPrefix(p.Text, "command=") {
					isV2 = true
				}
				break
			}
		}
	}
	var body []byte
	if isV2 {
		b, err := handleV2UploadPack(ctx.Store, repo, packets)
		if err != nil {
			panic(err)
		}
		body = b
	} else {
		body = handleV0UploadPack(ctx.Store, repo, packets)
	}
	sendBuffer(w, 200, body, uploadResultCT)
}

// ---------------------------------------------------------------------------
// receive-pack v0
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func parseReceivePackBody(ctx *Ctx) (commands []pushCommand, pack []byte, sideband bool, err error) {
	offset := 0
	packStart := len(ctx.Body)
	for offset+4 <= len(ctx.Body) {
		header := string(ctx.Body[offset : offset+4])
		var length int64
		length, err = parseHexLen(header)
		if err != nil {
			return nil, nil, false, apiErr(400, "bad pkt-line length in receive-pack")
		}
		if length == 0 {
			packStart = offset + 4
			offset = packStart
			break
		}
		if length < 4 || offset+int(length) > len(ctx.Body) {
			return nil, nil, false, apiErr(400, "truncated pkt-line in receive-pack")
		}
		text := string(ctx.Body[offset+4 : offset+int(length)])
		if nul := strings.Index(text, "\x00"); nul >= 0 {
			caps := text[nul+1:]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
			text = text[:nul]
		}
		text = strings.TrimSpace(text)
		fields := strings.Fields(text)
		if len(fields) != 3 || !isObjectId(fields[0]) || !isObjectId(fields[1]) {
			return nil, nil, false, apiErr(400, "bad receive-pack command line "+strconv.Quote(text))
		}
		commands = append(commands, pushCommand{oldOid: fields[0], newOid: fields[1], ref: fields[2]})
		offset += int(length)
	}
	return commands, ctx.Body[packStart:], sideband, nil
}

func parseHexLen(header string) (int64, error) {
	var n int64
	for _, c := range header {
		n *= 16
		switch {
		case c >= '0' && c <= '9':
			n += int64(c - '0')
		case c >= 'a' && c <= 'f':
			n += int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int64(c-'A') + 10
		default:
			return 0, fmt.Errorf("bad hex %q", header)
		}
	}
	return n, nil
}

// handleReceivePack serves POST /{owner}/{repo}.git/git-receive-pack.
func handleReceivePack(ctx *Ctx, w http.ResponseWriter) {
	repo, err := repoFromPath(ctx)
	if err != nil {
		panic(err)
	}
	commands, pack, sideband, err := parseReceivePackBody(ctx)
	if err != nil {
		panic(err)
	}
	var report []string
	unpackOK := true
	unpackDetail := ""
	if len(pack) > 0 {
		result, err := readPack(pack, func(oid string) *GitObject {
			if obj, ok := repo.Objects[oid]; ok {
				o := obj
				return &o
			}
			return nil
		})
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for oid, obj := range result.Objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = obj
				}
			}
		}
	}
	if !unpackOK {
		report = append(report, "unpack error "+unpackDetail+"\n")
	} else {
		report = append(report, "unpack ok\n")
		lookup := ctx.Store.Lookup(repo)
		for _, cmd := range commands {
			current, exists := repo.Refs[cmd.ref]
			if cmd.newOid == ZeroOid {
				// Delete.
				switch {
				case !exists:
					report = append(report, "ng "+cmd.ref+" remote ref does not exist\n")
				case cmd.oldOid != ZeroOid && cmd.oldOid != current:
					report = append(report, "ng "+cmd.ref+" old ref does not match\n")
				default:
					delete(repo.Refs, cmd.ref)
					report = append(report, "ok "+cmd.ref+"\n")
				}
				continue
			}
			if cmd.oldOid == ZeroOid {
				// Create.
				if exists {
					report = append(report, "ng "+cmd.ref+" already exists\n")
					continue
				}
				if _, ok := repo.Objects[cmd.newOid]; !ok {
					report = append(report, "ng "+cmd.ref+" object not found\n")
					continue
				}
				repo.Refs[cmd.ref] = cmd.newOid
				report = append(report, "ok "+cmd.ref+"\n")
				continue
			}
			// Update.
			switch {
			case !exists:
				report = append(report, "ng "+cmd.ref+" remote ref does not exist\n")
			case cmd.oldOid != current:
				report = append(report, "ng "+cmd.ref+" old ref does not match\n")
			case func() bool { _, ok := repo.Objects[cmd.newOid]; return !ok }():
				report = append(report, "ng "+cmd.ref+" object not found\n")
			case !isAncestor(lookup, current, cmd.newOid):
				report = append(report, "ng "+cmd.ref+" non-fast-forward\n")
			default:
				repo.Refs[cmd.ref] = cmd.newOid
				report = append(report, "ok "+cmd.ref+"\n")
			}
		}
	}
	var items []any
	for _, r := range report {
		items = append(items, r)
	}
	items = append(items, "flush")
	inner := encodePktLines(items)
	var body []byte
	if sideband {
		// The sideband packets carry the report; the outer pkt-line stream
		// still needs its own terminating flush, or the client sees a hangup.
		parts := sidebandPackets(inner, 1)
		parts = append(parts, pktFlush)
		body = bytes.Join(parts, nil)
	} else {
		body = inner
	}
	sendBuffer(w, 200, body, receiveResultCT)
}
