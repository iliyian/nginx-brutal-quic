// h3probe: minimal HTTP/3 benchmark client.
//
// Opens one QUIC connection and downloads over N concurrent streams, so the
// number reported is governed by the server's connection-level congestion
// controller — which is what this repo is about.
//
//	go run ./tools/h3probe -url https://host:8443/big -n 1 -secs 10
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func main() {
	var (
		url      = flag.String("url", "", "https:// URL to download")
		streams  = flag.Int("n", 1, "concurrent streams over one QUIC connection")
		secs     = flag.Float64("secs", 10, "max duration in seconds")
		insecure = flag.Bool("insecure", true, "skip TLS verification")
		label    = flag.String("label", "", "label echoed in the result line")
	)
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "missing -url")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(*secs*float64(time.Second)))
	defer cancel()

	rt := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: *insecure,
			NextProtos:         []string{"h3"},
			ServerName:         hostOf(*url),
		},
		QUICConfig: &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			KeepAlivePeriod: 10 * time.Second,
		},
	}
	defer rt.Close()

	var total, errs atomic.Int64

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < *streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
			defer rcancel()

			req, err := http.NewRequestWithContext(rctx, "GET", *url, nil)
			if err != nil {
				errs.Add(1)
				return
			}

			resp, err := rt.RoundTrip(req)
			if err != nil {
				errs.Add(1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				errs.Add(1)
				return
			}

			buf := make([]byte, 256*1024)
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					total.Add(int64(n))
				}
				if err != nil {
					if err != io.EOF {
						errs.Add(1)
					}
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}()
	}

	wg.Wait()

	elapsed := time.Since(start).Seconds()
	b := total.Load()

	tag := ""
	if *label != "" {
		tag = *label + " "
	}

	fmt.Printf("RESULT %sbytes=%d elapsed=%.3fs throughput=%.2f_Mbps streams=%d errors=%d\n",
		tag, b, elapsed, float64(b)*8/elapsed/1e6, *streams, errs.Load())
}

func hostOf(u string) string {
	s := strings.TrimPrefix(u, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		return s[:i]
	}
	return s
}
