package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed preview_page.html
var bannerPageHTML []byte

type bannerState struct {
	Report  *bannerReport `json:"report"`
	Failure string        `json:"failure"`
	Charge  string        `json:"charge"`
	Version int           `json:"version"`
	// BannerOrigin is where the banner files are served from: a second
	// listener, so the template runs on an origin of its own.
	BannerOrigin string `json:"banner_origin"`
}

// bannerServer serves the preview page and the prepared files of the current
// fill. The files on disk are never modified.
type bannerServer struct {
	mu      sync.RWMutex
	dir     string
	report  *bannerReport
	failure string
	version int
	old     []string // earlier folders, removed two swaps later

	subMu sync.Mutex
	subs  map[chan string]struct{}

	httpSrv    *http.Server // the page, /api/state and /events
	bannerSrv  *http.Server // only rows/<i>/..., on its own port and so its own origin
	bannerLn   net.Listener
	addr       string
	bannerAddr string
}

func newBannerServer() *bannerServer {
	return &bannerServer{subs: map[chan string]struct{}{}}
}

// Listen binds 127.0.0.1 only. port 0 picks a free one. A second listener on a
// free port of its own serves the banner files (see Handler / bannerHandler): a
// template's JavaScript runs there, on another origin than the page, so it can
// neither read /api/state nor touch the page.
func (s *bannerServer) Listen(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("poort %d is niet beschikbaar: %w", port, err)
	}
	bln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("geen vrije poort voor de banner: %w", err)
	}
	s.addr = ln.Addr().String()
	s.bannerAddr = bln.Addr().String()
	s.bannerLn = bln
	return ln, nil
}

func (s *bannerServer) URL() string { return "http://" + s.addr + "/" }

// BannerOrigin is the origin of the banner listener.
func (s *bannerServer) BannerOrigin() string { return "http://" + s.bannerAddr }

func (s *bannerServer) Serve(ln net.Listener) error {
	s.httpSrv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if s.bannerLn != nil {
		s.bannerSrv = &http.Server{Handler: s.bannerHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = s.bannerSrv.Serve(s.bannerLn) }()
	}
	err := s.httpSrv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close stops the server and removes every temp folder.
func (s *bannerServer) Close() {
	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.httpSrv.Shutdown(ctx)
		cancel()
		_ = s.httpSrv.Close()
	}
	if s.bannerSrv != nil {
		_ = s.bannerSrv.Close()
	} else if s.bannerLn != nil {
		_ = s.bannerLn.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range append(s.old, s.dir) {
		if d != "" {
			os.RemoveAll(d)
		}
	}
	s.old, s.dir = nil, ""
}

// Swap switches to a new fill and tells the pages to reload.
func (s *bannerServer) Swap(r *bannerResult) {
	s.mu.Lock()
	if s.dir != "" {
		s.old = append(s.old, s.dir)
	}
	for len(s.old) > 1 { // keep one previous folder: a page may still be loading from it
		os.RemoveAll(s.old[0])
		s.old = s.old[1:]
	}
	s.dir, s.report, s.failure = r.Dir, r.Report, ""
	s.version++
	v := s.version
	s.mu.Unlock()
	s.broadcast("reload", strconv.Itoa(v))
}

// Fail keeps the last good banner and shows the message over it.
func (s *bannerServer) Fail(msg string) {
	s.mu.Lock()
	s.failure = msg
	s.version++
	v := s.version
	s.mu.Unlock()
	s.broadcast("failure", strconv.Itoa(v))
}

func (s *bannerServer) state() bannerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return bannerState{Report: s.report, Charge: s.report.chargeText(), Failure: s.failure, Version: s.version, BannerOrigin: s.BannerOrigin()}
}

func (s *bannerServer) broadcast(event, data string) {
	msg := "event: " + event + "\ndata: " + data + "\n\n"
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Handler is the page origin: the page, its state and the reload events. It
// serves no banner file.
func (s *bannerServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/events", s.handleEvents)
	return localOnly(nosniff(mux))
}

// bannerHandler is the banner origin: only rows/<i>/... and nothing of /api/.
// It may only be framed by the page.
func (s *bannerServer) bannerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/rows/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors http://"+s.addr)
		s.handleFile(w, r)
	})
	return localOnly(nosniff(mux))
}

// nosniff stops browsers from guessing a content type.
func nosniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// localOnly refuses requests whose Host is not a loopback name, against DNS
// rebinding: the server is for this machine only.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1" {
			http.Error(w, "alleen bereikbaar via 127.0.0.1", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *bannerServer) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", bannerPageCSP(s.BannerOrigin()))
	_, _ = w.Write(bannerPageHTML)
}

func (s *bannerServer) handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.state())
}

func (s *bannerServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming niet mogelijk", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 8)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, ": verbonden\n\n")
	fl.Flush()
	keep := time.NewTicker(20 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			fmt.Fprint(w, m)
			fl.Flush()
		case <-keep.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *bannerServer) handleFile(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	dir, rep := s.dir, s.report
	s.mu.RUnlock()
	if dir == "" || rep == nil {
		http.NotFound(w, r)
		return
	}
	rel := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	rel = strings.TrimPrefix(rel, "/") // rows/<i>/...
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if !strings.HasPrefix(full, filepath.Clean(dir)+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	// rows/<i>/<entry>: the one file that gets the ready hook.
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) == 3 && path.Clean(parts[2]) == path.Clean(rep.Entry) {
		data, err := os.ReadFile(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		data = injectBridge(data)
		ct := mime.TypeByExtension(filepath.Ext(full))
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, filepath.Base(full), info.ModTime(), f)
}

// bannerBridge runs inside the banner, which the page frames with
// sandbox="allow-scripts" on another origin, so the page cannot reach into it.
// It talks through postMessage: it tells the page what the banner offers (ready
// meta tag, seek driver) and seeks when the page asks. The driver code is cut
// from the page script (between the driver:start and driver:end markers), the
// one place it is written and tested.
func bannerBridge() string {
	page := string(bannerPageHTML)
	i := strings.Index(page, "/* driver:start */")
	j := strings.Index(page, "/* driver:end */")
	driver := ""
	if i >= 0 && j > i {
		driver = page[i:j]
	}
	return `(function(){window.__taufinityReady=function(){try{parent.postMessage({type:"taufinity-ready"},"*")}catch(e){}};` +
		driver + `
var drv=null;
function announce(){
var uses=!!document.querySelector('meta[name="taufinity.ready"]');
try{drv=findDriver(window)}catch(e){drv=null}
try{parent.postMessage({type:"taufinity-driver",usesReady:uses,driver:drv?{kind:drv.kind,duration:drv.duration}:null},"*")}catch(e){}
}
if(document.readyState==="complete"){announce()}else{window.addEventListener("load",announce)}
window.addEventListener("message",function(ev){
if(ev.source!==parent||!ev.data||ev.data.type!=="taufinity-seek")return;
var t=Number(ev.data.t);if(!isFinite(t))return;
try{if(!drv)drv=findDriver(window);if(drv)drv.seek(t)}catch(e){}
});
})();`
}

// bannerReadyHook is the script put into the banner's entry file.
var bannerReadyHook = "<script>" + bannerBridge() + "</script>"

// bannerPageCSP is the policy of the page: no inline script except the page's
// own (by hash), frames only from the banner origin, nothing from elsewhere.
func bannerPageCSP(bannerOrigin string) string {
	return "default-src 'self'; script-src " + inlineHash(bannerPageHTML, "script") +
		"; style-src " + inlineHash(bannerPageHTML, "style") +
		"; frame-src " + bannerOrigin + "; img-src 'self' data:; base-uri 'none'; form-action 'none'; object-src 'none'"
}

// inlineHash is the CSP hash source of the first inline <tag> element.
func inlineHash(html []byte, tag string) string {
	open := bytes.Index(html, []byte("<"+tag+">"))
	if open < 0 {
		return "'none'"
	}
	start := open + len(tag) + 2
	end := bytes.Index(html[start:], []byte("</"+tag+">"))
	if end < 0 {
		return "'none'"
	}
	sum := sha256.Sum256(html[start : start+end])
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

var (
	metaTagRE  = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	headOpenRE = regexp.MustCompile(`(?is)<head(?:\s[^>]*)?>`)
	bodyOpenRE = regexp.MustCompile(`(?is)<body(?:\s[^>]*)?>`)
	htmlOpenRE = regexp.MustCompile(`(?is)<html(?:\s[^>]*)?>`)
	attrRE     = regexp.MustCompile(`(?is)([a-z\-:]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// hasReadyMeta reports whether the file carries
// <meta name="taufinity.ready" content="signal">.
func hasReadyMeta(html []byte) bool {
	for _, tag := range metaTagRE.FindAll(html, -1) {
		var name, content string
		for _, m := range attrRE.FindAllSubmatch(tag, -1) {
			v := string(m[2]) + string(m[3]) + string(m[4])
			switch strings.ToLower(string(m[1])) {
			case "name":
				name = v
			case "content":
				content = v
			}
		}
		if strings.EqualFold(name, "taufinity.ready") && strings.EqualFold(content, "signal") {
			return true
		}
	}
	return false
}

// injectBridge puts the bridge (which holds the ready hook) into the entry file,
// whatever the file says.
func injectBridge(html []byte) []byte {
	at := 0
	if loc := headOpenRE.FindIndex(html); loc != nil {
		at = loc[1]
	} else if loc := htmlOpenRE.FindIndex(html); loc != nil {
		at = loc[1]
	} else if !hasReadyMeta(html) && !bodyOpenRE.Match(html) {
		return html // not a web page: leave it as it is
	}
	var out bytes.Buffer
	out.Grow(len(html) + len(bannerReadyHook))
	out.Write(html[:at])
	out.WriteString(bannerReadyHook)
	out.Write(html[at:])
	return out.Bytes()
}

// injectReadyHook returns the served bytes: unchanged, unless the file has the
// ready meta tag, then the hook goes in right after <head>.
func injectReadyHook(html []byte) []byte {
	if !hasReadyMeta(html) {
		return html
	}
	return injectBridge(html)
}
