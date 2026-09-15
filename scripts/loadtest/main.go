// Command loadtest submits code to a running OCEE instance at a fixed
// concurrency, waits for every result, and reports throughput and latency
// percentiles.
//
// Latency here is end-to-end from the caller's point of view: enqueue, queue
// wait, container start, execution, and the poll that observes the result.
// That is the number a user of the API actually feels, and it is deliberately
// larger than the per-submission execution time the engine reports.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	latency time.Duration
	status  string
	err     error
}

func main() {
	var (
		base        = flag.String("url", "http://localhost:8080", "base URL of the API")
		language    = flag.String("lang", "python", "language id")
		total       = flag.Int("n", 100, "number of submissions")
		concurrency = flag.Int("c", 10, "concurrent in-flight submissions")
		pollEvery   = flag.Duration("poll", 200*time.Millisecond, "result poll interval")
		timeout     = flag.Duration("timeout", 2*time.Minute, "per-submission deadline")
	)
	flag.Parse()

	code := defaultCode(*language)
	if code == "" {
		fmt.Fprintf(os.Stderr, "no built-in program for language %q\n", *language)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	results := make([]result, *total)

	var completed atomic.Int64
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup

	fmt.Printf("submitting %d %s jobs at concurrency %d against %s\n\n", *total, *language, *concurrency, *base)
	start := time.Now()

	for i := 0; i < *total; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()

			t0 := time.Now()
			r := runOne(ctx, client, *base, *language, code, *pollEvery)
			r.latency = time.Since(t0)
			results[i] = r

			if n := completed.Add(1); n%10 == 0 {
				fmt.Printf("  %d/%d done\n", n, *total)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	report(results, elapsed, *concurrency)
}

func runOne(ctx context.Context, c *http.Client, base, language, code string, poll time.Duration) result {
	body, _ := json.Marshal(map[string]string{"language": language, "code": code})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/submissions", bytes.NewReader(body))
	if err != nil {
		return result{err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return result{err: err}
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return result{err: fmt.Errorf("submit returned %s: %s", resp.Status, raw)}
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return result{err: err}
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return result{err: fmt.Errorf("timed out waiting for %s", created.ID)}
		case <-ticker.C:
		}

		greq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/submissions/"+created.ID, nil)
		if err != nil {
			return result{err: err}
		}
		gresp, err := c.Do(greq)
		if err != nil {
			return result{err: err}
		}
		graw, _ := io.ReadAll(gresp.Body)
		gresp.Body.Close()

		var got struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(graw, &got); err != nil {
			return result{err: err}
		}
		if got.Status == "completed" || got.Status == "failed" {
			return result{status: got.Status}
		}
	}
}

func report(results []result, elapsed time.Duration, concurrency int) {
	var ok []time.Duration
	failures := map[string]int{}

	for _, r := range results {
		switch {
		case r.err != nil:
			failures[r.err.Error()]++
		case r.status != "completed":
			failures["status="+r.status]++
		default:
			ok = append(ok, r.latency)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i] < ok[j] })

	fmt.Printf("\n%s\n", "=== results ===")
	fmt.Printf("total          %d\n", len(results))
	fmt.Printf("succeeded      %d\n", len(ok))
	fmt.Printf("failed         %d\n", len(results)-len(ok))
	fmt.Printf("concurrency    %d\n", concurrency)
	fmt.Printf("wall clock     %s\n", elapsed.Round(time.Millisecond))
	if elapsed > 0 {
		fmt.Printf("throughput     %.2f submissions/sec\n", float64(len(ok))/elapsed.Seconds())
	}

	if len(ok) > 0 {
		var sum time.Duration
		for _, d := range ok {
			sum += d
		}
		fmt.Printf("\nend-to-end latency (submit -> result visible)\n")
		fmt.Printf("  min          %s\n", ok[0].Round(time.Millisecond))
		fmt.Printf("  mean         %s\n", (sum / time.Duration(len(ok))).Round(time.Millisecond))
		fmt.Printf("  p50          %s\n", percentile(ok, 0.50).Round(time.Millisecond))
		fmt.Printf("  p95          %s\n", percentile(ok, 0.95).Round(time.Millisecond))
		fmt.Printf("  p99          %s\n", percentile(ok, 0.99).Round(time.Millisecond))
		fmt.Printf("  max          %s\n", ok[len(ok)-1].Round(time.Millisecond))
	}

	if len(failures) > 0 {
		fmt.Printf("\nfailures\n")
		for msg, n := range failures {
			fmt.Printf("  %4d  %s\n", n, msg)
		}
		os.Exit(1)
	}
}

// percentile uses nearest-rank on the sorted slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// defaultCode is a short CPU-bound program per language, so the measurement
// reflects container overhead plus a little real work rather than pure startup.
func defaultCode(language string) string {
	switch language {
	case "python":
		return "print(sum(i*i for i in range(200000)))"
	case "node":
		return "let s=0; for(let i=0;i<200000;i++) s+=i*i; console.log(s);"
	case "c":
		return "#include <stdio.h>\nint main(void){long long s=0;for(long i=0;i<200000;i++)s+=(long long)i*i;printf(\"%lld\\n\",s);return 0;}"
	case "cpp":
		return "#include <iostream>\nint main(){long long s=0;for(long i=0;i<200000;i++)s+=(long long)i*i;std::cout<<s<<std::endl;}"
	case "java":
		return "public class Main{public static void main(String[] a){long s=0;for(int i=0;i<200000;i++)s+=(long)i*i;System.out.println(s);}}"
	}
	return ""
}
