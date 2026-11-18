package main

// Process lifecycle for the forge entry, ported from entry/main.ts.
//
// The grading env owns the trial: it tells the entry where it lives through
// MAKEFUN_ENV_URL, the entry answers with the ephemeral port its HTTP server
// bound, and then the entry simply stays alive until the env reports the
// trial finished. Only then may the process exit, with code 0.
//
// Contract, step by step:
//   - No MAKEFUN_ENV_URL: nothing to register with, exit 1 immediately.
//   - The server binds port 0 (kernel-assigned). Port 8080 belongs to the env
//     and is never bound here.
//   - POST {env}/register with {"port": N}. A 200 records this entry; a 409
//     means a first registration already won and is equally final. A 400 or
//     405 is a contract error and aborts the process. Anything else (the env
//     may still be booting) is retried with capped exponential backoff.
//   - GET {env}/done is polled; the env answers {"done": false} while the
//     suite runs and {"done": true} once it is over. Failed polls are just
//     "not done yet" and are retried the same way.
//   - On done: close the server and exit 0 so the harness can collect the
//     trial export.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

const forgeCallTimeout = 30 * time.Second
const forgeBackoffFirstMs = 250
const forgeBackoffCapMs = 2000

func forgePause(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// Delay before retry number `attempt`: 250ms doubling, capped at 2s.
func forgeBackoffMs(attempt int) int {
	shift := attempt
	if shift > 8 {
		shift = 8
	}
	if shift < 0 {
		shift = 0
	}
	ms := forgeBackoffFirstMs << uint(shift)
	if ms > forgeBackoffCapMs {
		ms = forgeBackoffCapMs
	}
	return ms
}

// Tell the env which port the server bound; retries until accepted.
func forgeAnnouncePort(envOrigin string, port int) error {
	client := &http.Client{Timeout: forgeCallTimeout}
	for attempt := 0; ; attempt++ {
		accepted, fatal := forgeTryRegister(client, envOrigin, port)
		if fatal != nil {
			return fatal
		}
		if accepted {
			return nil
		}
		forgePause(forgeBackoffMs(attempt))
	}
}

// One register attempt: (true, nil) when accepted, a non-nil error only for
// the fatal 400/405 contract refusal; anything else is retryable.
func forgeTryRegister(client *http.Client, envOrigin string, port int) (bool, error) {
	body, _ := json.Marshal(map[string]int{"port": port})
	req, err := http.NewRequest(http.MethodPost, envOrigin+"/register", bytes.NewReader(body))
	if err != nil {
		return false, nil
	}
	req.Header.Set("content-type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return false, nil
	}
	defer res.Body.Close()
	if res.StatusCode == 200 || res.StatusCode == 409 {
		io.Copy(io.Discard, res.Body)
		return true, nil
	}
	if res.StatusCode == 400 || res.StatusCode == 405 {
		text, _ := io.ReadAll(res.Body)
		return false, fmt.Errorf("register refused with HTTP %d: %s", res.StatusCode, string(text))
	}
	io.Copy(io.Discard, res.Body)
	return false, nil
}

// Block until the env's /done endpoint answers {"done": true}.
func forgeAwaitTrialEnd(envOrigin string) {
	client := &http.Client{Timeout: forgeCallTimeout}
	for attempt := 0; ; attempt++ {
		done := false
		func() {
			res, err := client.Get(envOrigin + "/done")
			if err != nil {
				return
			}
			defer res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				var body struct {
					Done interface{} `json:"done"`
				}
				if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
					return
				}
				if b, ok := body.Done.(bool); ok && b {
					done = true
				}
				return
			}
			io.Copy(io.Discard, res.Body)
		}()
		if done {
			return
		}
		// Env unreachable or the poll timed out: the trial is not over, poll again.
		forgePause(forgeBackoffMs(attempt))
	}
}

func forgeRun(envOrigin string) error {
	server := CreateServer()
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return errors.New("server did not bind an ephemeral TCP port")
	}
	go func() {
		// Serve errors after the listener is closed (trial end) are expected.
		_ = server.Serve(listener)
	}()
	fmt.Printf("[forge] http server listening on port %d\n", addr.Port)

	if err := forgeAnnouncePort(envOrigin, addr.Port); err != nil {
		return err
	}
	fmt.Print("[forge] registered with the env; waiting for the trial to end\n")

	forgeAwaitTrialEnd(envOrigin)
	server.Close()
	fmt.Print("[forge] trial over; shutting down\n")
	return nil
}

func main() {
	// The trial is short-lived and allocation-heavy (JSON, pkt-lines, pack
	// buffers) with 1024 MiB of RAM: run the GC far less often than the
	// default percent so collection does not tax every workload.
	debug.SetGCPercent(800)
	debug.SetMemoryLimit(768 << 20)
	// The trial pod is capped at 2 CPUs (see the kit's RULES.txt); pin the
	// scheduler to that so thread/GC-worker counts cannot oversubscribe
	// the sandbox if CPU detection under gVisor reports the whole node.
	runtime.GOMAXPROCS(2)
	envOrigin := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envOrigin == "" {
		fmt.Fprint(os.Stderr, "[forge] MAKEFUN_ENV_URL missing: no env to register with, exiting\n")
		os.Exit(1)
	}
	if err := forgeRun(envOrigin); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] %s\n", err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}
