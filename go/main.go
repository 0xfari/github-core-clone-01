// main.go — the entry <-> env lifecycle: bind an ephemeral port, tell the
// env which port it is, then poll until the env says the trial is over and
// exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing
//      to talk to, so the program exits 1 at once.
//   2. Create the server and listen(0): the kernel picks a free port. Never
//      bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the
//      address the request came from plus N as the base URL and dials it
//      from then on. The first register wins; a 409 means another entry
//      already registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is a
//      poll, not a long-held request. A failed or slow poll is retried with
//      a short exponential backoff; the process never exits before the env
//      says done.
//   5. Close the server and exit 0. The harness then stops the env, which
//      writes the export the scorer reads.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// How long one register or done request may take before it is abandoned.
const requestTimeout = 30 * time.Second

// The first wait between retries, and the longest.
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

// retryDelay is the wait after `attempt` failed requests: exponential,
// capped.
func retryDelay(attempt int) time.Duration {
	d := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

// register POSTs the bound port until the env accepts it. 200 is the normal
// answer; 409 means a first register already won, which is not this
// process's to fix. 400 and 405 are contract errors and end the program;
// a network error or another status is retried, since the env may still be
// starting.
func register(client *http.Client, envURL string, port int) error {
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		body, _ := json.Marshal(map[string]int{"port": port})
		req, err := http.NewRequestWithContext(ctx, "POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		cancel()
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}; never
// returns before that.
func waitForDone(client *http.Client, envURL string) {
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, "GET", envURL+"/done", nil)
		if err == nil {
			res, err := client.Do(req)
			if err == nil {
				var body struct {
					Done bool `json:"done"`
				}
				if decErr := json.NewDecoder(res.Body).Decode(&body); decErr == nil && body.Done {
					res.Body.Close()
					cancel()
					return
				}
				res.Body.Close()
			}
		}
		cancel()
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}

	server := &http.Server{Handler: &appServer{store: NewStore()}}
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "[entry] the server did not bind an ephemeral TCP port")
		os.Exit(1)
	}
	fmt.Printf("[entry] listening on port %d\n", addr.Port)

	// A clean shutdown if the harness stops the process mid-trial: close
	// the server and leave with 0 rather than dying with a dangling socket.
	shuttingDown := false
	shutdown := func() {
		if shuttingDown {
			return
		}
		shuttingDown = true
		server.Close()
		go func() {
			time.Sleep(time.Second)
			os.Exit(0)
		}()
	}
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		shutdown()
	}()

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
			os.Exit(1)
		}
	}()

	client := &http.Client{}
	if err := register(client, envURL, addr.Port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	waitForDone(client, envURL)
	server.Close()
	fmt.Println("[entry] done")
}
