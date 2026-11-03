// Command driver is the user load and the operator of the moimroom E2E test
// (test/e2e/moimroom/run.sh).
//
// Users, through the gateway: writers book one-hour slots (POST
// /api/reservations), a host uploads photos (POST /api/host/spaces/{id}/photos),
// readers open spaces and the uploaded photos. Every booking that succeeded is
// recorded in the platform's written-records stream.
//
// Operator, through the platform API only: submit the plan and upload the
// target manifest (agent token), approve the reviewed revision, start with a
// canary and an observe phase, approve the switch (operator token).
//
// After the run it checks the result itself: every recorded booking is on the
// new primary, and every uploaded photo is served by the new environment with
// the same bytes (so the files moved). It writes the run id to -run-id-file.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

const table = "app.reservations"

var kst = time.FixedZone("KST", 9*60*60)

func main() {
	apiURL := flag.String("api", "", "platform API URL")
	gwURL := flag.String("gateway", "", "gateway public URL")
	planPath := flag.String("plan", "test/e2e/moimroom/plan.json", "engine plan to submit")
	manifestPath := flag.String("manifest", "test/e2e/moimroom/target.yaml", "target CNPG Cluster, uploaded as attachment target.yaml")
	stream := flag.String("stream", "moimroom", "written-records stream the plan verifies")
	writers := flag.Int("writers", 4, "concurrent booking users")
	readers := flag.Int("readers", 4, "concurrent reading users")
	photoEvery := flag.Duration("photo-every", 2*time.Second, "how often the host uploads a photo")
	canaryFlag := flag.String("canary", "10,50", "canary read weights to the target")
	canaryStep := flag.Duration("canary-step", 10*time.Second, "duration of each canary step")
	observe := flag.Duration("observe", 30*time.Second, "observe phase after the cutover (0 = off)")
	runIDFile := flag.String("run-id-file", "", "write the run id here")
	timeout := flag.Duration("timeout", 20*time.Minute, "give up after this long")
	flag.Parse()
	log.SetFlags(log.Ltime)
	agentTok, opTok := os.Getenv("E2E_AGENT_TOKEN"), os.Getenv("E2E_OPERATOR_TOKEN")
	targetDSN, hostCode := os.Getenv("E2E_TARGET_DSN"), os.Getenv("E2E_HOST_CODE")
	if *apiURL == "" || *gwURL == "" || agentTok == "" || opTok == "" || targetDSN == "" || hostCode == "" {
		log.Fatal("need -api, -gateway, E2E_AGENT_TOKEN, E2E_OPERATOR_TOKEN, E2E_TARGET_DSN and E2E_HOST_CODE")
	}
	var weights []int
	for _, s := range strings.Split(*canaryFlag, ",") {
		w, err := strconv.Atoi(strings.TrimSpace(s))
		must(err, "-canary")
		weights = append(weights, w)
	}
	raw, err := os.ReadFile(*planPath)
	must(err, "read plan")
	manifest, err := os.ReadFile(*manifestPath)
	must(err, "read manifest")

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	agent, op := client.New(*apiURL, agentTok), client.New(*apiURL, opTok)
	began := time.Now()

	l := newLoad(*gwURL, hostCode)
	must(l.loadSpaces(ctx), "list spaces through the gateway")
	stopLoad := l.start(*writers, *readers, *photoEvery, agent, *stream)
	for {
		st, err := agent.GetWrittenRecordStats(ctx, *stream)
		if err == nil && st.Count > 0 && l.photoOK.Load() > 0 {
			break
		}
		if ctx.Err() != nil {
			log.Fatalf("no bookings reached stream %s or no photo uploaded (last error: %v)", *stream, err)
		}
		time.Sleep(time.Second)
	}
	log.Printf("users running through %s: %d bookers, %d readers, a photo every %s, %d spaces", *gwURL, *writers, *readers, *photoEvery, len(l.spaces))

	pr, err := agent.SubmitPlan(ctx, raw)
	must(err, "submit plan")
	att, err := agent.PutPlanAttachment(ctx, pr.Ref, "target.yaml", manifest)
	must(err, "upload target.yaml")
	pr, err = op.GetPlan(ctx, pr.Ref)
	must(err, "get plan")
	log.Printf("plan %s submitted (agent), target.yaml %d bytes, revision %d", pr.Ref, att.Size, pr.Revision)
	_, err = op.ApprovePlan(ctx, pr.Ref, "e2e: reviewed target.yaml", pr.Revision)
	must(err, "approve plan")
	step := canaryStep.String()
	req := apiv1.StartMigrationRequest{PlanRef: pr.Ref, Canary: &apiv1.CanaryOptions{Weights: weights, StepDuration: &step}}
	if *observe > 0 {
		d, s, no := observe.String(), "10s", false
		req.Observe = &apiv1.ObserveOptions{Duration: &d, StepDuration: &s, AutoRollback: &no}
	}
	started, err := op.StartMigration(ctx, req, "")
	must(err, "start migration")
	log.Printf("plan approved (operator); run %s started with canary %v every %s, observe %s", started.RunId, weights, step, *observe)
	if *runIDFile != "" {
		must(os.WriteFile(*runIDFile, []byte(started.RunId), 0o644), "write run id")
	}

	progress := time.NewTicker(15 * time.Second)
	defer progress.Stop()
	go func() {
		for range progress.C {
			log.Printf("   users: %s", l.counts())
		}
	}()

	switched := false
	err = op.Watch(ctx, started.RunId, func(m client.Message) error {
		status := ""
		if m.Snapshot != nil {
			status = string(m.Snapshot.Status)
		}
		if e := m.Event; e != nil {
			fmt.Printf("%s  %-14s %-10s %s\n", e.Time.Local().Format("15:04:05"), e.Kind, deref(e.Step), deref(e.Message))
			if e.Kind == "status_changed" && e.Data != nil {
				status, _ = (*e.Data)["status"].(string)
			}
		}
		if status == string(apiv1.RunStatusAwaitingSwitch) && !switched {
			switched = true
			if _, err := op.SwitchMigration(ctx, started.RunId); err != nil {
				return fmt.Errorf("approve switch: %w", err)
			}
			log.Printf("switch approved (operator)")
		}
		return nil
	})
	must(err, "watch run")

	time.Sleep(5 * time.Second) // users keep using the new environment for a moment
	stopLoad()
	run, err := op.GetMigration(ctx, started.RunId)
	must(err, "get run")
	st, err := agent.GetWrittenRecordStats(ctx, *stream)
	must(err, "written-record stats")

	ok := report(ctx, run, l, st.Count, targetDSN)
	fmt.Printf("driver wall    %s (load start to end)\n", time.Since(began).Round(time.Second))
	if !ok {
		fmt.Println("== migration FAILED")
		os.Exit(1)
	}
	fmt.Println("== migration passed")
}

func report(ctx context.Context, run *apiv1.Run, l *load, streamCount int64, targetDSN string) bool {
	res := run.Result
	ok := true
	fail := func(format string, a ...any) {
		ok = false
		fmt.Printf("FAIL: "+format+"\n", a...)
	}
	fmt.Println("== result")
	fmt.Printf("run            %s status=%s %s\n", run.RunId, run.Status, deref(run.StatusReason))
	if run.Canary != nil {
		for _, s := range run.Canary.Steps {
			if s.Gate != nil {
				fmt.Printf("canary %3d%%    passed=%t %s\n", s.Weight, s.Gate.Passed, s.Gate.Reason)
			}
		}
	}
	if o := run.Observe; o != nil {
		for _, s := range o.Steps {
			reason := ""
			if s.Gate != nil {
				reason = s.Gate.Reason
			}
			fmt.Printf("observe        %-12s %s\n", s.Verdict, reason)
		}
		fmt.Printf("observe        passed=%s violation=%s\n", boolStr(o.Passed), deref(o.Violation))
	}
	fmt.Printf("downtime       %s (writes held at the gateway, engine)\n", millis(res.DowntimeMillis))
	fmt.Printf("run duration   %s\n", millis(res.DurationMillis))
	fmt.Printf("gateway        requests=%s failed=%s (during the run)\n", num(res.Requests), num(res.FailedRequests))
	fmt.Printf("verify         verified=%s missingRows=%s missingWrites=%s missingFiles=%s\n",
		boolStr(res.Verified), num(res.MissingRows), num(res.MissingWrites), num(res.MissingFiles))
	fmt.Printf("bookings       ok=%d failed=%d conflict=%d; longest gap without a booking %s, slowest %s\n",
		l.writeOK.Load(), l.writeFail.Load(), l.conflict.Load(), l.maxGap.Round(time.Millisecond), l.slowest.Round(time.Millisecond))
	fmt.Printf("photos         uploaded=%d failed=%d\n", l.photoOK.Load(), l.photoFail.Load())
	fmt.Printf("reads          ok=%d notFound=%d failed=%d; by backend %v\n",
		l.readOK.Load(), l.readMissing.Load(), l.readFail.Load(), l.byBackend)
	fmt.Printf("records        sent=%d unsent=%d stream=%d\n", l.sent, len(l.unsent), streamCount)

	if run.Status != apiv1.RunStatusSucceeded {
		fail("run status is %s, want succeeded", run.Status)
	}
	if res.MissingWrites == nil || *res.MissingWrites != 0 {
		fail("run result missingWrites is %s, want 0", num(res.MissingWrites))
	}
	if res.MissingFiles == nil || *res.MissingFiles != 0 {
		fail("run result missingFiles is %s, want 0 (rclone check)", num(res.MissingFiles))
	}
	if len(l.unsent) > 0 || streamCount != l.sent {
		fail("written records did not all reach the platform (sent %d, unsent %d, stream %d)", l.sent, len(l.unsent), streamCount)
	}
	if run.Status != apiv1.RunStatusSucceeded {
		return ok
	}
	found, err := countOnTarget(ctx, targetDSN, l.ids)
	if err != nil {
		fail("count recorded bookings on the new primary: %v", err)
	} else {
		fmt.Printf("recheck        %d of %d recorded bookings are on the new primary\n", found, len(l.ids))
		if found != int64(len(l.ids)) {
			fail("%d recorded bookings are missing on the new primary", int64(len(l.ids))-found)
		}
	}
	same, served, missing := l.checkPhotos()
	fmt.Printf("photo recheck  %d of %d uploaded photos served by the new environment with the same bytes (%d missing)\n", same, len(l.photos), missing)
	if same != len(l.photos) {
		fail("%d uploaded photos are not served correctly after the migration (served %d)", len(l.photos)-same, served)
	}
	return ok
}

// countOnTarget counts the recorded bookings present on the new primary,
// including the ones made after the engine's verify step.
func countOnTarget(ctx context.Context, dsn string, ids []int64) (int64, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	var n int64
	err = conn.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE id = ANY($1)", ids).Scan(&n)
	return n, err
}

// --- users ---

type space struct {
	ID        int64 `json:"id"`
	OpenHour  int   `json:"openHour"`
	CloseHour int   `json:"closeHour"`
}

type slot struct {
	space int64
	date  string
	hour  int
}

type photo struct {
	space, id int64
	size      int
}

type load struct {
	gw, hostCode string
	hc           *http.Client
	spaces       []space
	slots        []slot
	next         atomic.Int64 // index into slots

	mu        sync.Mutex
	ids       []int64 // bookings that succeeded
	photos    []photo // uploads that succeeded
	unsent    []apiv1.WrittenRecord
	sent      int64
	lastOK    time.Time
	maxGap    time.Duration
	slowest   time.Duration
	byBackend map[string]int64
	failLogs  int

	writeOK, writeFail, conflict, photoOK, photoFail, readOK, readMissing, readFail atomic.Int64
}

func newLoad(gw, hostCode string) *load {
	return &load{
		gw:       strings.TrimRight(gw, "/"),
		hostCode: hostCode,
		// Held writes wait up to the gateway's max hold (55s by default).
		hc:        &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: 64}},
		byBackend: map[string]int64{},
	}
}

// loadSpaces reads the seeded spaces and lays out every bookable one-hour
// slot from tomorrow on, so no two bookings collide.
func (l *load) loadSpaces(ctx context.Context) error {
	var body struct {
		Items []space `json:"items"`
	}
	for {
		code, b, _, err := l.do(http.MethodGet, "/api/spaces?limit=100", "", nil)
		if err == nil && code == http.StatusOK && json.Unmarshal(b, &body) == nil && len(body.Items) > 0 {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("status %d, err %v", code, err)
		}
		time.Sleep(time.Second)
	}
	l.spaces = body.Items
	today := time.Now().In(kst)
	for day := 1; day <= 55; day++ {
		date := today.AddDate(0, 0, day).Format(time.DateOnly)
		for _, s := range l.spaces {
			for h := s.OpenHour; h < s.CloseHour; h++ {
				l.slots = append(l.slots, slot{s.ID, date, h})
			}
		}
	}
	rand.Shuffle(len(l.slots), func(i, j int) { l.slots[i], l.slots[j] = l.slots[j], l.slots[i] })
	return nil
}

func (l *load) start(writers, readers int, photoEvery time.Duration, agent *client.Client, stream string) func() {
	stop := make(chan struct{})
	var users, rec sync.WaitGroup
	for range writers {
		users.Go(func() { l.booker(stop) })
	}
	for range readers {
		users.Go(func() { l.reader(stop) })
	}
	users.Go(func() { l.host(stop, photoEvery) })
	done := make(chan struct{})
	rec.Go(func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				l.flush(agent, stream)
			case <-done:
				l.flush(agent, stream)
				return
			}
		}
	})
	return func() {
		close(stop)
		users.Wait()
		close(done)
		rec.Wait()
	}
}

const pace = 50 * time.Millisecond // between one user's requests

func (l *load) booker(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		i := l.next.Add(1) - 1
		if int(i) >= len(l.slots) {
			return // out of slots
		}
		s := l.slots[i]
		body := fmt.Sprintf(`{"spaceId":%d,"date":%q,"startHour":%d,"endHour":%d,"headcount":1,"guestName":"e2e","phone":"010-1234-5678"}`,
			s.space, s.date, s.hour, s.hour+1)
		t0 := time.Now()
		code, b, _, err := l.do(http.MethodPost, "/api/reservations", body, nil)
		var r struct {
			ID int64 `json:"id"`
		}
		switch {
		case err == nil && code == http.StatusCreated && json.Unmarshal(b, &r) == nil && r.ID > 0:
			l.booked(r.ID, t0, time.Now())
		case err == nil && code == http.StatusConflict:
			l.conflict.Add(1) // a slot booked twice would be a bug of this driver, not the migration
		default:
			l.writeFail.Add(1)
			l.logFail("booking", code, err, b)
		}
		time.Sleep(pace)
	}
}

// host uploads a small, unique PNG to a random space every interval.
func (l *load) host(stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		sp := l.spaces[rand.IntN(len(l.spaces))].ID
		img, ctype, body := pngForm(n)
		code, b, _, err := l.do(http.MethodPost, fmt.Sprintf("/api/host/spaces/%d/photos", sp), body, map[string]string{
			"Content-Type": ctype, "X-Host-Code": l.hostCode,
		})
		var p struct {
			ID int64 `json:"id"`
		}
		if err == nil && code == http.StatusCreated && json.Unmarshal(b, &p) == nil && p.ID > 0 {
			l.photoOK.Add(1)
			l.mu.Lock()
			l.photos = append(l.photos, photo{space: sp, id: p.ID, size: img})
			l.mu.Unlock()
		} else {
			l.photoFail.Add(1)
			l.logFail("photo upload", code, err, b)
		}
	}
}

// pngForm returns a multipart body with one PNG whose pixels encode n, so
// every photo has different bytes (and a different checksum).
func pngForm(n int) (int, string, string) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range 64 {
		img.Set(i%8, i/8, color.RGBA{uint8(n), uint8(n >> 8), uint8(i * 4), 255})
	}
	var pb bytes.Buffer
	_ = png.Encode(&pb, img)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("photo", fmt.Sprintf("e2e-%d.png", n))
	_, _ = fw.Write(pb.Bytes())
	_ = w.Close()
	return pb.Len(), w.FormDataContentType(), body.String()
}

func (l *load) reader(stop <-chan struct{}) {
	for n := 0; ; n++ {
		select {
		case <-stop:
			return
		default:
		}
		path := fmt.Sprintf("/api/spaces/%d", l.spaces[rand.IntN(len(l.spaces))].ID)
		if n%2 == 1 {
			l.mu.Lock()
			if len(l.photos) > 0 {
				p := l.photos[rand.IntN(len(l.photos))]
				path = fmt.Sprintf("/api/spaces/%d/photos/%d", p.space, p.id)
			}
			l.mu.Unlock()
		}
		code, b, backend, err := l.do(http.MethodGet, path, "", nil)
		switch {
		case err == nil && code == http.StatusOK:
			l.readOK.Add(1)
		case err == nil && code == http.StatusNotFound:
			l.readMissing.Add(1) // the target a moment behind during the canary (row or file)
		default:
			l.readFail.Add(1)
			l.logFail("read "+path, code, err, b)
		}
		if backend != "" {
			l.mu.Lock()
			l.byBackend[backend]++
			l.mu.Unlock()
		}
		time.Sleep(pace)
	}
}

// checkPhotos fetches every uploaded photo through the gateway (now the new
// environment) and compares the size with what was uploaded.
func (l *load) checkPhotos() (same, served, missing int) {
	for _, p := range l.photos {
		code, b, _, err := l.do(http.MethodGet, fmt.Sprintf("/api/spaces/%d/photos/%d", p.space, p.id), "", nil)
		switch {
		case err == nil && code == http.StatusOK:
			served++
			if len(b) == p.size {
				same++
			}
		case err == nil && code == http.StatusNotFound:
			missing++
		}
	}
	return
}

func (l *load) do(method, path, body string, hdr map[string]string) (int, []byte, string, error) {
	req, err := http.NewRequest(method, l.gw+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, resp.Header.Get("X-Charlie-Backend"), nil
}

func (l *load) booked(id int64, t0, now time.Time) {
	l.writeOK.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append(l.ids, id)
	l.unsent = append(l.unsent, apiv1.WrittenRecord{Table: table, Pk: strconv.FormatInt(id, 10), At: &now})
	if !l.lastOK.IsZero() && now.Sub(l.lastOK) > l.maxGap {
		l.maxGap = now.Sub(l.lastOK)
	}
	if now.After(l.lastOK) {
		l.lastOK = now
	}
	l.slowest = max(l.slowest, now.Sub(t0))
}

func (l *load) flush(agent *client.Client, stream string) {
	l.mu.Lock()
	batch := l.unsent
	l.unsent = nil
	l.mu.Unlock()
	for len(batch) > 0 {
		n := min(len(batch), 5000)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := agent.AppendWrittenRecords(ctx, stream, batch[:n])
		cancel()
		if err != nil {
			log.Printf("send written records: %v", err)
			l.mu.Lock()
			l.unsent = append(batch, l.unsent...)
			l.mu.Unlock()
			return
		}
		l.mu.Lock()
		l.sent += int64(n)
		l.mu.Unlock()
		batch = batch[n:]
	}
}

func (l *load) logFail(kind string, code int, err error, body []byte) {
	l.mu.Lock()
	l.failLogs++
	n := l.failLogs
	l.mu.Unlock()
	if n <= 20 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 160 {
			msg = msg[:160]
		}
		log.Printf("   %s failed: status=%d err=%v %s", kind, code, err, msg)
	}
}

func (l *load) counts() string {
	return fmt.Sprintf("bookings ok=%d failed=%d, photos ok=%d failed=%d, reads ok=%d notFound=%d failed=%d",
		l.writeOK.Load(), l.writeFail.Load(), l.photoOK.Load(), l.photoFail.Load(), l.readOK.Load(), l.readMissing.Load(), l.readFail.Load())
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s: %v", what, err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func num(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

func millis(v *int64) string {
	if v == nil {
		return "-"
	}
	return (time.Duration(*v) * time.Millisecond).String()
}

func boolStr(v *bool) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatBool(*v)
}
