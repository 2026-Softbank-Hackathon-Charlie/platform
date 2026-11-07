package fileguard

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type measuredBody struct {
	io.ReadCloser
	bytes *atomic.Int64
}

func (b measuredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes.Add(int64(n))
	return n, err
}

type measuredTransport struct{ requests, received, sent *atomic.Int64 }

func (t measuredTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.requests.Add(1)
	if r.Body != nil {
		r.Body = measuredBody{r.Body, t.sent}
	}
	response, err := http.DefaultTransport.RoundTrip(r)
	if response != nil && response.Body != nil {
		response.Body = measuredBody{response.Body, t.received}
	}
	return response, err
}

// This uses real rclone processes, without any inspection history. OS/backend
// caches are not flushed. Each sample starts with an empty destination.
func BenchmarkColdTransfer(b *testing.B) {
	if os.Getenv("FILEGUARD_COLD_BENCH") != "1" {
		b.Skip("opt-in real rclone benchmark")
	}
	for _, kind := range []string{"small", "large", "zip", "denied"} {
		b.Run(kind, func(b *testing.B) {
			var requests, received, sent atomic.Int64
			if endpoint := os.Getenv("FILEGUARD_MINIO_ENDPOINT"); endpoint != "" {
				u, err := url.Parse(endpoint)
				if err != nil {
					b.Fatal(err)
				}
				proxy := httputil.NewSingleHostReverseProxy(u)
				proxy.ErrorLog = log.New(io.Discard, "", 0)
				proxy.Transport = measuredTransport{&requests, &received, &sent}
				server := httptest.NewServer(proxy)
				defer server.Close()
				b.Setenv("RCLONE_CONFIG_BENCH_ENDPOINT", server.URL)
			}
			source := b.TempDir()
			count, size := 32, 256
			if kind == "large" {
				count, size = 4, 1<<20
			}
			if kind == "zip" {
				count, size = 4, 256<<10
			}
			for i := 0; i < count; i++ {
				data := bytes.Repeat([]byte("x"), size)
				name := fmt.Sprintf("file-%03d.txt", i)
				if kind == "zip" {
					var buf bytes.Buffer
					writer := zip.NewWriter(&buf)
					header := &zip.FileHeader{Name: "text.txt", Method: zip.Deflate}
					f, err := writer.CreateHeader(header)
					if err != nil {
						b.Fatal(err)
					}
					if _, err = f.Write(data); err != nil {
						b.Fatal(err)
					}
					if err = writer.Close(); err != nil {
						b.Fatal(err)
					}
					data = buf.Bytes()
					name += ".zip"
				}
				if err := os.WriteFile(filepath.Join(source, name), data, 0600); err != nil {
					b.Fatal(err)
				}
			}
			if kind == "denied" {
				if err := os.WriteFile(filepath.Join(source, "zz-late.txt"), []byte("synthetic@example.invalid"), 0600); err != nil {
					b.Fatal(err)
				}
			}
			if remote := os.Getenv("FILEGUARD_BENCH_SOURCE"); remote != "" {
				target := remote + "/" + kind
				if err := run(context.Background(), io.Discard, "sync", source, target, "--ignore-times"); err != nil {
					b.Fatal(err)
				}
				source = target
			}
			b.ResetTimer()
			for range b.N {
				destination := b.TempDir()
				if remote := os.Getenv("FILEGUARD_BENCH_TARGET"); remote != "" {
					destination = remote + "/" + kind
					if err := run(context.Background(), io.Discard, "purge", destination); err != nil {
						b.Fatal(err)
					}
				}
				var mu sync.Mutex
				commands, downloads, uploads := 0, 0, 0
				var list, upload, compare time.Duration
				var downloadBegin, downloadEnd time.Time
				stageRoot := ""
				var stagePeak atomic.Int64
				stopMeasure, measured := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(measured)
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stopMeasure:
							return
						case <-ticker.C:
							mu.Lock()
							root := stageRoot
							mu.Unlock()
							if root == "" {
								continue
							}
							var total int64
							_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
								if err == nil && !d.IsDir() {
									if info, e := d.Info(); e == nil {
										total += info.Size()
									}
								}
								return nil
							})
							for old := stagePeak.Load(); total > old; old = stagePeak.Load() {
								if stagePeak.CompareAndSwap(old, total) {
									break
								}
							}
						}
					}
				}()
				syncer := New(source, destination, nil, "", func(context.Context) error { return nil }, func(_ context.Context, r Report) error {
					if slices.Contains(r.Labels, "kr-only") {
						return ErrDenied
					}
					return nil
				})
				syncer.execute = func(ctx context.Context, out io.Writer, args ...string) error {
					start := time.Now()
					mu.Lock()
					candidate := ""
					if args[0] == "copy" {
						candidate = args[2]
					}
					if args[0] == "cat" {
						if bounded, ok := out.(*boundedWriter); ok {
							if file, ok := bounded.w.(*os.File); ok {
								candidate = filepath.Dir(file.Name())
							}
						}
					}
					if candidate != "" {
						for !strings.HasPrefix(filepath.Base(candidate), "hopcharlie-inspect-") && candidate != filepath.Dir(candidate) {
							candidate = filepath.Dir(candidate)
						}
						stageRoot = candidate
					}
					commands++
					if args[0] == "cat" || args[0] == "copy" {
						downloads++
						if downloadBegin.IsZero() {
							downloadBegin = start
						}
					}
					if args[0] == "sync" {
						uploads++
					}
					mu.Unlock()
					err := run(ctx, out, args...)
					end := time.Now()
					mu.Lock()
					defer mu.Unlock()
					switch args[0] {
					case "lsjson":
						list += end.Sub(start)
					case "cat", "copy":
						if end.After(downloadEnd) {
							downloadEnd = end
						}
					case "sync":
						upload += end.Sub(start)
					case "check":
						compare += end.Sub(start)
					}
					return err
				}
				start := time.Now()
				requests.Store(0)
				received.Store(0)
				sent.Store(0)
				_, err := syncer.Sync(context.Background(), source, destination)
				close(stopMeasure)
				<-measured
				if kind == "denied" {
					if err != ErrDenied || uploads != 0 {
						b.Fatal("denial did not prevent upload")
					}
				} else if err != nil {
					b.Fatal(err)
				}
				elapsed := time.Since(start)
				b.ReportMetric(float64(commands), "commands/op")
				b.ReportMetric(float64(downloads), "downloads/op")
				b.ReportMetric(float64(requests.Load()), "remote-requests/op")
				b.ReportMetric(float64(received.Load()), "remote-response-bytes/op")
				b.ReportMetric(float64(sent.Load()), "remote-request-bytes/op")
				b.ReportMetric(float64(stagePeak.Load()), "sampled-stage-peak-bytes")
				b.ReportMetric(float64(list.Microseconds())/1000, "list-ms/op")
				b.ReportMetric(float64(downloadEnd.Sub(downloadBegin).Microseconds())/1000, "download-ms/op")
				b.ReportMetric(float64(upload.Microseconds())/1000, "upload-ms/op")
				b.ReportMetric(float64(compare.Microseconds())/1000, "compare-ms/op")
				other := elapsed - list - downloadEnd.Sub(downloadBegin) - upload - compare
				b.ReportMetric(float64(other.Microseconds())/1000, "scan-verify-stage-ms/op")
				var usage unix.Rusage
				if unix.Getrusage(unix.RUSAGE_SELF, &usage) == nil {
					b.ReportMetric(float64(usage.Maxrss), "peak-rss-kib")
				}
				if unix.Getrusage(unix.RUSAGE_CHILDREN, &usage) == nil {
					b.ReportMetric(float64(usage.Maxrss), "max-child-rss-kib")
				}
				var memory runtime.MemStats
				runtime.ReadMemStats(&memory)
				b.ReportMetric(float64(memory.Sys), "go-sys-bytes")
			}
		})
	}
}
