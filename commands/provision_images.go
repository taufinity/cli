// provision_images.go — provision support for seeding image knowledge
// files from a manifest of source URLs.
//
// Flow per entry:
//  1. download bytes from source_url
//  2. name-based dedupe BEFORE upload (no classify cost on re-runs):
//     if an image KnowledgeFile with the same derived name already
//     exists in the org, skip
//  3. resize down to download_max_dim longest edge if larger (cheap
//     storage + keeps payload under the 25 MB upload cap)
//  4. POST multipart to /api/knowledge-files (the real upload endpoint;
//     never a direct DB write)
//  5. the org's router rule (source=knowledge_file, mime~image/*) then
//     auto-triggers the classify_image playbook asynchronously
//
// Idempotent: re-running on an already-seeded org is a NOOP (step 2).
//
// ground_truth_tags in the manifest are for offline eval only — they are
// NOT sent to the classify step (that's the model's job).
//
// Plan reference: cto-as-a-service/docs/plans/2026-05-13-efteling-image-asset-poc.md
package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

// imagesManifestConfig is the YAML shape for the demo image corpus.
// See studio/images/manifest.yaml in the customer template.
//
//	defaults:
//	  download_max_dim: 1600
//	  source_attribution_required: true
//	images:
//	  - source_url: https://upload.wikimedia.org/.../symbolica.jpg
//	    source_attribution: "Wikimedia / CC-BY-SA-4.0 / <photographer>"
//	    ground_truth_tags:
//	      attraction: [symbolica]
//	      season: autumn
type imagesManifestConfig struct {
	Defaults imagesManifestDefaults `yaml:"defaults,omitempty"`
	Images   []imagesManifestEntry  `yaml:"images,omitempty"`
}

type imagesManifestDefaults struct {
	DownloadMaxDim            int  `yaml:"download_max_dim,omitempty"`
	SourceAttributionRequired bool `yaml:"source_attribution_required,omitempty"`
}

type imagesManifestEntry struct {
	SourceURL         string         `yaml:"source_url"`
	SourceAttribution string         `yaml:"source_attribution,omitempty"`
	GroundTruthTags   map[string]any `yaml:"ground_truth_tags,omitempty"`
	// DownloadMaxDim overrides defaults.download_max_dim for this entry only.
	DownloadMaxDim int `yaml:"download_max_dim,omitempty"`
}

// Sanity range for download_max_dim. Catches operator typos
// (e.g. 160000 instead of 1600) before Phase B5 launches a runaway
// download. Real model-side resize is separately bounded in the
// classify_image step (Phase B3) at 768px.
const (
	manifestMinDim = 64
	manifestMaxDim = 8192
)

// imageDownloadTimeout bounds a single source_url fetch. Wikimedia
// originals can be a few MB; 60s is generous without hanging a CI run.
const imageDownloadTimeout = 60 * time.Second

// kbListResponse mirrors the JSON envelope returned by GET /api/knowledge-files.
// We only care about a handful of fields per file.
type kbListResponse struct {
	Files []kbListItem `json:"files"`
}

type kbListItem struct {
	ID       uint        `json:"id"`
	UUID     string      `json:"uuid"`
	Name     string      `json:"name"`
	FileType string      `json:"file_type"`
	Purpose  string      `json:"purpose"`
	Tags     []kbTagItem `json:"tags"`
}

type kbTagItem struct {
	Name string `json:"name"`
}

// upsertImagesManifest downloads each manifest image, dedupes against
// already-seeded files by name, resizes oversized images, and uploads
// via the real knowledge-files endpoint. The org's router rule then
// triggers classification asynchronously.
func upsertImagesManifest(c *provisionClient, orgID uint, cfg imagesManifestConfig) error {
	if len(cfg.Images) == 0 {
		fmt.Printf("images-manifest org=%d: manifest empty — nothing to seed\n", orgID)
		return nil
	}

	if cfg.Defaults.DownloadMaxDim != 0 {
		if cfg.Defaults.DownloadMaxDim < manifestMinDim || cfg.Defaults.DownloadMaxDim > manifestMaxDim {
			return fmt.Errorf("images-manifest defaults.download_max_dim %d out of range [%d, %d]",
				cfg.Defaults.DownloadMaxDim, manifestMinDim, manifestMaxDim)
		}
	}

	// Validate each entry up front; fail before any download.
	seenURL := map[string]int{}
	for i, e := range cfg.Images {
		if strings.TrimSpace(e.SourceURL) == "" {
			return fmt.Errorf("images-manifest entry[%d]: source_url is required", i)
		}
		u, err := url.Parse(e.SourceURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("images-manifest entry[%d]: source_url %q must be an absolute http(s) URL", i, e.SourceURL)
		}
		if cfg.Defaults.SourceAttributionRequired && strings.TrimSpace(e.SourceAttribution) == "" {
			return fmt.Errorf("images-manifest entry[%d] (%s): source_attribution required by defaults", i, e.SourceURL)
		}
		if e.DownloadMaxDim != 0 && (e.DownloadMaxDim < manifestMinDim || e.DownloadMaxDim > manifestMaxDim) {
			return fmt.Errorf("images-manifest entry[%d]: download_max_dim %d out of range [%d, %d]",
				i, e.DownloadMaxDim, manifestMinDim, manifestMaxDim)
		}
		if prev, dup := seenURL[e.SourceURL]; dup {
			return fmt.Errorf("images-manifest entry[%d]: duplicate source_url %q (first seen at entry[%d])",
				i, e.SourceURL, prev)
		}
		seenURL[e.SourceURL] = i
	}

	defaultDim := cfg.Defaults.DownloadMaxDim
	if defaultDim == 0 {
		defaultDim = 1600
	}

	if c.dryRun {
		fmt.Printf("[dry-run] images-manifest org=%d (entries=%d, default_max_dim=%d)\n", orgID, len(cfg.Images), defaultDim)
		for i, e := range cfg.Images {
			fmt.Printf("  [dry-run] image[%d] download+upload: %s\n", i, e.SourceURL)
		}
		return nil
	}

	// Dedupe set: names of image files already in the org.
	existing, err := listExistingImageNames(c, orgID)
	if err != nil {
		return fmt.Errorf("images-manifest: list existing: %w", err)
	}

	fmt.Printf("images-manifest org=%d (entries=%d, default_max_dim=%d)\n", orgID, len(cfg.Images), defaultDim)
	var uploaded, skipped int
	for i, e := range cfg.Images {
		name := imageNameFromURL(e.SourceURL)
		if _, seen := existing[strings.ToLower(name)]; seen {
			fmt.Printf("  SKIP image[%d] %s — already seeded\n", i, name)
			skipped++
			continue
		}

		dim := e.DownloadMaxDim
		if dim == 0 {
			dim = defaultDim
		}
		data, mime, err := downloadAndResize(c, e.SourceURL, dim)
		if err != nil {
			c.Warn("image[%d] %s: %v", i, e.SourceURL, err)
			continue
		}

		// POST to the no-slash path (same as the web client, web/src/lib/api.ts
		// uploadKnowledgeFile). The trailing-slash form is NOT covered by the
		// Cloudflare upload-allow WAF rule, so real-photo uploads to it 403 on
		// an OWASP false-positive. Matching the frontend keeps one code path.
		respBody, status, err := c.uploadMultipartForOrg("/knowledge-files", "file", name, data, orgID)
		if err != nil || status >= 300 {
			c.Warn("image[%d] %s: upload status=%d err=%v body=%s", i, name, status, err, provisionSummarize(respBody))
			continue
		}
		var created struct {
			UUID string `json:"uuid"`
		}
		_ = json.Unmarshal(respBody, &created)
		fmt.Printf("  UPLOAD image[%d] %s (%d bytes, %s) uuid=%s — classify will run via router rule\n",
			i, name, len(data), mime, created.UUID)
		uploaded++
	}

	fmt.Printf("images-manifest org=%d: uploaded=%d skipped=%d (classification runs async; check the Image Library)\n",
		orgID, uploaded, skipped)
	return nil
}

// listExistingImageNames returns the lowercase names of image knowledge
// files already in the org, for name-based dedupe.
func listExistingImageNames(c *provisionClient, orgID uint) (map[string]struct{}, error) {
	body, status, err := c.getForOrg("/knowledge-files?content_type=image&limit=1000", orgID)
	if err != nil || status != 200 {
		return nil, fmt.Errorf("status=%d err=%v body=%s", status, err, provisionSummarize(body))
	}
	var list kbListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		if err2 := json.Unmarshal(body, &list.Files); err2 != nil {
			return nil, fmt.Errorf("parse list: %v / %v", err, err2)
		}
	}
	out := make(map[string]struct{}, len(list.Files))
	for _, f := range list.Files {
		out[strings.ToLower(f.Name)] = struct{}{}
	}
	return out, nil
}

// imageNameFromURL derives a stable filename from a source URL. Wikimedia
// thumbnail URLs end in ".../NNNpx-Real_Name.jpg"; we take the basename
// and strip a leading "NNNpx-" thumbnail prefix so the name matches the
// underlying file (and dedupe is stable across thumbnail sizes).
func imageNameFromURL(srcURL string) string {
	u, err := url.Parse(srcURL)
	if err != nil {
		return srcURL
	}
	base := path.Base(u.Path)
	if decoded, err := url.PathUnescape(base); err == nil {
		base = decoded
	}
	// Strip "640px-" / "1600px-" thumbnail prefixes.
	if idx := strings.Index(base, "px-"); idx > 0 && idx <= 5 {
		base = base[idx+3:]
	}
	return base
}

// downloadAndResize fetches the image and, if its longest edge exceeds
// maxDim, scales it down (re-encoding JPEG at q85). PNGs are decoded and
// re-encoded as JPEG when resized; small-enough images pass through
// untouched with their original bytes + content type.
func downloadAndResize(c *provisionClient, srcURL string, maxDim int) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, srcURL, nil)
	if err != nil {
		return nil, "", err
	}
	// Wikimedia blocks the default Go UA; identify politely.
	req.Header.Set("User-Agent", "taufinity-provision/1.0 (demo image seeding; contact hello@taufinity.io)")
	hc := &http.Client{Timeout: imageDownloadTimeout}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("download status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 30<<20)) // 30 MB hard cap
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// Not decodable as image — refuse rather than upload garbage.
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return raw, mime, nil // small enough; keep original bytes
	}

	// Scale longest edge to maxDim, preserving aspect.
	nw, nh := w, h
	if w >= h {
		nw = maxDim
		nh = int(float64(h) * float64(maxDim) / float64(w))
	} else {
		nh = maxDim
		nw = int(float64(w) * float64(maxDim) / float64(h))
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", fmt.Errorf("re-encode: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

func applyImagesManifest(c *provisionClient, dir string, orgID uint) error {
	mf := filepath.Join(dir, "images", "manifest.yaml")
	if !fileExists(mf) {
		return nil
	}
	var cfg imagesManifestConfig
	mustReadYAML(mf, &cfg)
	return upsertImagesManifest(c, orgID, cfg)
}
