// The entry <-> env lifecycle: bind an ephemeral port, tell the env the
// port it bound, then poll until the env says the trial is over and exit 0.
// This is the same lifecycle the kit starter's main.ts runs, ported to Go so
// the hot path (the server) is native and Node is not in the loop at all.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing
//      to talk to, so the program exits 1 at once.
//   2. Listen on port 0: the kernel picks a free port. Never bind 8080.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. 200 is normal; 409 means
//      another entry already registered. 400 and 405 are contract errors and
//      end the program; other failures are retried with exponential backoff.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}.
//   5. Close the server and exit 0.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// How long one register or done request may take before it is abandoned and retried.
const requestTimeout = 30 * time.Second

// The first wait between retries, and the longest.
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

// retryDelay is the wait after `attempt` failed requests: exponential from
// retryInitial, capped at retryMax.
func retryDelay(attempt int) time.Duration {
	shift := attempt
	if shift > 8 {
		shift = 8
	}
	d := retryInitial * time.Duration(1<<uint(shift))
	if d > retryMax {
		d = retryMax
	}
	return d
}

// register POSTs the bound port until the env accepts it.
func register(envURL string, port int) error {
	client := &http.Client{Timeout: requestTimeout}
	body := fmt.Sprintf(`{"port":%d}`, port)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			respBody, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(respBody)))
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}; it never
// returns before that.
func waitForDone(envURL string) {
	client := &http.Client{Timeout: requestTimeout}
	for attempt := 0; ; attempt++ {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			body, readErr := io.ReadAll(res.Body)
			res.Body.Close()
			if readErr == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
				var parsed struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(body, &parsed) == nil && parsed.Done {
					return
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	// Fewer GC pauses on the latency-bound workloads; the heap stays small
	// (well under the pod's 1024 MB) either way.
	debug.SetGCPercent(200)

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}

	forge := newForge()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[entry] "+err.Error())
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: &app{forge: forge}}
	go func() {
		_ = server.Serve(ln)
	}()
	fmt.Printf("[entry] listening on port %d\n", port)

	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "[entry] "+err.Error())
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	_ = server.Close()
	fmt.Println("[entry] done")
}
