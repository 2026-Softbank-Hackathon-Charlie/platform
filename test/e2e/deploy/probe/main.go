// Command probe sends one GET request after another, every -every, until the
// file -stop exists, then writes what it saw to -out as JSON and a summary to
// stdout. run.sh runs it against the gateway while it deploys.
//
//	probe -url http://127.0.0.1:18300/ -host demo.e2e.test -stop stop -out result.json [-only body] [-last body]
//
// A request fails when it gets no response or a status other than 200. The
// bodies of the 200 responses are counted by their first line (the demo
// app's version), with the time each was first and last seen. The exit code
// is 1 when a request failed, when -only is set and a 200 had another body,
// or when -last is set and the last 200 had another body.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type body struct {
	Count     int     `json:"count"`
	FirstSeen float64 `json:"firstSeenSeconds"` // since the probe started
	LastSeen  float64 `json:"lastSeenSeconds"`
}

type result struct {
	Requests int              `json:"requests"`
	Failed   int              `json:"failed"`
	Statuses map[string]int   `json:"statuses"` // of the failed requests; "error" for no response
	Errors   []string         `json:"errors,omitempty"`
	Bodies   map[string]*body `json:"bodies"`
	Seconds  float64          `json:"seconds"`
}

func main() {
	url := flag.String("url", "", "URL to request")
	host := flag.String("host", "", "Host header (default: the URL's host)")
	every := flag.Duration("every", 50*time.Millisecond, "time between the starts of two requests")
	stop := flag.String("stop", "", "stop once this file exists")
	out := flag.String("out", "", "write the result here")
	only := flag.String("only", "", "every 200 must have this body")
	last := flag.String("last", "", "the last 200 must have this body")
	flag.Parse()
	if *url == "" || *stop == "" || *out == "" {
		log.Fatal("-url, -stop and -out are required")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	res := result{Statuses: map[string]int{}, Bodies: map[string]*body{}}
	lastBody := ""
	began := time.Now()
	fail := func(status, msg string) {
		res.Failed++
		res.Statuses[status]++
		if len(res.Errors) < 10 {
			res.Errors = append(res.Errors, fmt.Sprintf("%.1fs %s", time.Since(began).Seconds(), msg))
		}
	}
	for tick := time.NewTicker(*every); ; <-tick.C {
		if _, err := os.Stat(*stop); err == nil {
			break
		}
		res.Requests++
		req, err := http.NewRequest(http.MethodGet, *url, nil)
		if err != nil {
			log.Fatal(err)
		}
		if *host != "" {
			req.Host = *host
		}
		resp, err := client.Do(req)
		if err != nil {
			fail("error", err.Error())
			continue
		}
		first, _ := bufio.NewReader(io.LimitReader(resp.Body, 4096)).ReadString('\n')
		resp.Body.Close()
		first = strings.TrimSpace(first)
		if resp.StatusCode != http.StatusOK {
			fail(fmt.Sprint(resp.StatusCode), fmt.Sprintf("%d %s", resp.StatusCode, first))
			continue
		}
		at := time.Since(began).Seconds()
		b := res.Bodies[first]
		if b == nil {
			b = &body{FirstSeen: at}
			res.Bodies[first] = b
		}
		b.Count++
		b.LastSeen = at
		lastBody = first
	}
	res.Seconds = time.Since(began).Seconds()
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out+".tmp", data, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(*out+".tmp", *out); err != nil {
		log.Fatal(err)
	}

	summary := fmt.Sprintf("requests=%d failed=%d over %.1fs", res.Requests, res.Failed, res.Seconds)
	names := make([]string, 0, len(res.Bodies))
	for name := range res.Bodies {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return res.Bodies[names[i]].FirstSeen < res.Bodies[names[j]].FirstSeen })
	for _, name := range names {
		b := res.Bodies[name]
		summary += fmt.Sprintf("; %q x%d (%.1fs to %.1fs)", name, b.Count, b.FirstSeen, b.LastSeen)
	}
	fmt.Println(summary)
	ok := res.Failed == 0
	if *only != "" && (len(names) != 1 || names[0] != *only) {
		fmt.Printf("want only %q\n", *only)
		ok = false
	}
	if *last != "" && lastBody != *last {
		fmt.Printf("the last answer was %q, want %q\n", lastBody, *last)
		ok = false
	}
	if !ok {
		os.Exit(1)
	}
}
