package commands

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/archive"
	"github.com/taufinity/cli/internal/auth"
	"github.com/taufinity/cli/internal/config"
)

var templateCmd = &cobra.Command{
	Use:   "template",
	Short: "Template commands",
	Long:  `Commands for working with templates - preview, validate, upload.`,
}

var templateHelpCmd = &cobra.Command{
	Use:   "help-syntax",
	Short: "Show template syntax reference",
	Long:  `Show a reference guide for template syntax, available functions, and data structure.`,
	Run:   runTemplateHelp,
}

var templatePreviewCmd = &cobra.Command{
	Use:   "preview [article-id]",
	Short: "Preview a template render",
	Long: `Upload local templates and preview a rendered article.

This command:
1. Creates an archive of local template files (respecting .gitignore)
2. Uploads the archive to the API
3. Triggers a render job
4. Polls for completion
5. Opens the result in your browser

Examples:
  # Preview with random article
  taufinity template preview

  # Preview specific article
  taufinity template preview 123

  # Preview without opening browser
  taufinity template preview --no-open
`,
	RunE: runTemplatePreview,
}

var (
	noOpen     bool
	outputFile string
)

func init() {
	rootCmd.AddCommand(templateCmd)
	templateCmd.AddCommand(templatePreviewCmd)
	templateCmd.AddCommand(templateHelpCmd)

	templatePreviewCmd.Flags().BoolVar(&noOpen, "no-open", false, "Don't open browser, just print URL")
	templatePreviewCmd.Flags().StringVarP(&outputFile, "output", "o", "", "Save rendered HTML to file")
}

func runTemplatePreview(cmd *cobra.Command, args []string) error {
	// Check authentication
	if !auth.HasCredentials() {
		return fmt.Errorf("not authenticated. Run 'taufinity auth login' first")
	}

	// Parse article ID if provided
	var articleID uint
	if len(args) > 0 {
		id, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil {
			return fmt.Errorf("invalid article ID: %s", args[0])
		}
		articleID = uint(id)
	}

	// Find project root and load config (where taufinity.yaml is, or current dir)
	projectDir, err := findProjectDir()
	if err != nil {
		return fmt.Errorf("find project: %w", err)
	}

	// Load project config and apply site from taufinity.yaml
	projectCfg, err := config.LoadProject(projectDir)
	if err != nil {
		return fmt.Errorf("load project config: %w", err)
	}
	for _, w := range projectCfg.Warnings {
		Print("⚠️  %s\n", w)
	}
	if projectCfg.Site != "" {
		SetSite(projectCfg.Site)
	}

	// Get site (from flags, env, user config, or project yaml)
	site := GetSite()
	if site == "" {
		return fmt.Errorf("no site configured. Set 'site' in taufinity.yaml or run 'taufinity config set site SITE_ID'")
	}

	// Create client (will auto-refresh token if needed). Forward the
	// resolved --org override so template preview can target sites in
	// orgs the session token didn't auth into directly. The X-Organization-ID
	// header lets the server-side cross-org check pass when the user has
	// membership in the target org.
	client := api.New(GetAPIURL())
	client.SetDebug(IsDebug())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}

	// Pre-flight auth check with auto-refresh
	Print("🔐 Checking authentication...\n")
	if err := client.ValidateAuth(context.Background()); err != nil {
		return fmt.Errorf("authentication failed: %w\nRun 'taufinity auth login' to re-authenticate", err)
	}

	Print("📁 Project: %s\n", projectDir)
	Print("🌐 Site: %s\n", site)
	if articleID > 0 {
		Print("📄 Article: %d\n", articleID)
	} else {
		Print("📄 Article: (random)\n")
	}

	// Step 1: Create archive
	Print("\n⏳ Creating template archive...\n")

	archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("taufinity-preview-%d.tar.gz", time.Now().UnixNano()))
	defer os.Remove(archivePath)

	if err := archive.Create(projectDir, archivePath); err != nil {
		return fmt.Errorf("create archive: %w", err)
	}

	archiveInfo, _ := os.Stat(archivePath)
	Print("   ✓ Archive created: %d bytes\n", archiveInfo.Size())

	// Step 2: Upload archive
	Print("⏳ Uploading archive...\n")

	archiveID, err := uploadArchive(client, archivePath)
	if err != nil {
		return fmt.Errorf("upload archive: %w", err)
	}
	Print("   ✓ Uploaded: %s\n", archiveID)

	// Step 3: Start render job
	Print("⏳ Starting render job...\n")

	renderResult, err := startPreviewRender(client, archiveID, articleID, site, IsDebug())
	if err != nil {
		return fmt.Errorf("start render: %w", err)
	}
	Print("   ✓ Job started: %s\n", renderResult.JobID)

	// Show warnings from data resolution
	for _, w := range renderResult.Warnings {
		Print("   ⚠ %s\n", w)
	}

	// Step 4: Poll for completion
	Print("⏳ Waiting for render...")

	job, err := pollJobCompletion(client, renderResult.JobID, 2*time.Minute)
	if err != nil {
		Print("\n")
		return fmt.Errorf("render failed: %w", err)
	}
	Print("\n   ✓ Render complete (%dms)\n", job.DurationMs)

	// Step 5: Get result
	if len(job.Files) == 0 {
		return fmt.Errorf("no output files generated")
	}

	// Find the main HTML file
	mainFile := "article.html"
	for _, f := range job.Files {
		if filepath.Ext(f.Filename) == ".html" {
			mainFile = f.Filename
			break
		}
	}

	// Build URL for the rendered file
	resultURL := fmt.Sprintf("%s/api/render-jobs/%s/files/%s", GetAPIURL(), renderResult.JobID, mainFile)

	if outputFile != "" {
		// Download and save to file
		Print("⏳ Downloading result...\n")
		if err := downloadFile(client, renderResult.JobID, mainFile, outputFile); err != nil {
			return fmt.Errorf("download file: %w", err)
		}
		Print("   ✓ Saved to: %s\n", outputFile)
	}

	Print("\n✅ Preview ready!\n")
	Print("   URL: %s\n", resultURL)

	if !noOpen && !IsQuiet() {
		Print("\n🌐 Opening in browser...\n")
		if err := browser.OpenURL(resultURL); err != nil {
			Print("   (Could not open browser automatically)\n")
		}
	}

	return nil
}

func findProjectDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	projectDir, err := config.FindProjectRoot(cwd)
	if err != nil {
		return "", fmt.Errorf("no taufinity.yaml found in %s (or parent directories)\n\nCreate one with:\n\n  site: your_site_id\n  template: templates/page.html", cwd)
	}

	return projectDir, nil
}

func uploadArchive(client *api.Client, archivePath string) (string, error) {
	// Read archive
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		return "", err
	}

	// Create multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("archive", filepath.Base(archivePath))
	if err != nil {
		return "", err
	}
	if _, err := part.Write(archiveData); err != nil {
		return "", err
	}
	writer.Close()

	// Upload
	resp, err := client.PostMultipart(context.Background(), "/api/templates/upload", &buf, writer.FormDataContentType())
	if err != nil {
		return "", err
	}
	if !resp.IsSuccess() {
		return "", fmt.Errorf("upload failed: %s", string(resp.Body))
	}

	var result struct {
		ArchiveID string `json:"archive_id"`
	}
	if err := resp.DecodeJSON(&result); err != nil {
		return "", err
	}

	return result.ArchiveID, nil
}

type previewRenderResult struct {
	JobID    string   `json:"job_id"`
	Warnings []string `json:"warnings"`
}

func startPreviewRender(client *api.Client, archiveID string, articleID uint, siteID string, debug bool) (*previewRenderResult, error) {
	payload := map[string]any{
		"archive_id": archiveID,
		"site_id":    siteID,
	}
	if articleID > 0 {
		payload["article_id"] = articleID
	}
	if debug {
		payload["debug"] = true
	}

	resp, err := client.PostJSONWithAuth(context.Background(), "/api/preview/render", payload)
	if err != nil {
		return nil, err
	}
	if !resp.IsSuccess() {
		return nil, fmt.Errorf("start render failed: %s", string(resp.Body))
	}

	var result previewRenderResult
	if err := resp.DecodeJSON(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

type jobStatus struct {
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error"`
	Files      []struct {
		Filename string `json:"filename"`
	} `json:"files"`
}

func pollJobCompletion(client *api.Client, jobID string, timeout time.Duration) (*jobStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for render")
		case <-ticker.C:
			Print(".")

			resp, err := client.GetWithAuth(ctx, "/api/render-jobs/"+jobID)
			if err != nil {
				return nil, err
			}
			if !resp.IsSuccess() {
				return nil, fmt.Errorf("get job status: %s", string(resp.Body))
			}

			var job jobStatus
			if err := resp.DecodeJSON(&job); err != nil {
				return nil, err
			}

			switch job.Status {
			case "completed":
				return &job, nil
			case "failed":
				return nil, fmt.Errorf("render failed: %s", job.Error)
			case "pending", "running":
				continue
			default:
				return nil, fmt.Errorf("unknown job status: %s", job.Status)
			}
		}
	}
}

func downloadFile(client *api.Client, jobID, filename, destPath string) error {
	resp, err := client.GetWithAuth(context.Background(), fmt.Sprintf("/api/render-jobs/%s/files/%s", jobID, filename))
	if err != nil {
		return err
	}
	if !resp.IsSuccess() {
		return fmt.Errorf("download failed: %s", string(resp.Body))
	}

	return os.WriteFile(destPath, resp.Body, 0644)
}

func runTemplateHelp(_ *cobra.Command, _ []string) {
	fmt.Print(`TEMPLATE SYNTAX REFERENCE
=========================

Taufinity uses Go's html/template engine. Templates are HTML files with
{{ ... }} tags that get replaced with data at render time.

PROJECT SETUP (taufinity.yaml)
------------------------------

  site: my_site_id              # Site to pull article data from
  template: templates/page.html # Entry template (relative path)
  preview_data: test-data/preview.json  # Optional: static preview data

  ignore:                       # Optional: files to exclude from archive
    - "*.test.html"
    - "dev/"

The 'preview_data' field points to a JSON file with sample data for previewing
templates that use custom schemas. Without it, the preview uses article data
from the database or a generic fallback.

DATA STRUCTURE
--------------

Standard article data provides these top-level objects:

  {{.Article.Title}}        Article title
  {{.Article.Subtitle}}     Article subtitle
  {{.Article.Content}}      Article body (raw markdown)
  {{.Article.ContentHTML}}  Article body (rendered HTML)
  {{.Article.Slug}}         URL slug
  {{.Article.Language}}     Language code (e.g. "nl")
  {{.Article.MetaDesc}}     Meta description
  {{.Article.Keywords}}     Keywords
  {{.Article.PublishedAt}}  Publication date (RFC3339)

  {{.Translation.Content}}      Translated content (raw markdown)
  {{.Translation.ContentHTML}}  Translated content (rendered HTML)

  {{.Site.Name}}            Site name
  {{.Site.Domain}}          Site domain
  {{.Site.SiteID}}          Site identifier

Custom data (from article_meta or preview_data) lives under {{.Custom}}
to keep it clearly separated from standard fields.

CUSTOM DATA (article_meta / preview_data)
-----------------------------------------

Custom data is always nested under {{.Custom}}. Standard keys (Article,
Translation, Site) stay at the top level. Everything else is accessed
via {{.Custom.KeyName.SubKey}}.

Example 1: Quote page

  preview.json:
    {
      "Quote": {
        "Text": "Be yourself; everyone else is taken.",
        "Theme": "Authenticity",
        "MatchScore": 92
      },
      "Author": {
        "Name": "Oscar Wilde",
        "JobTitle": "Writer",
        "URL": "https://en.wikipedia.org/wiki/Oscar_Wilde"
      }
    }

  template:
    <h1>{{.Custom.Quote.Text}}</h1>
    <p>Theme: {{.Custom.Quote.Theme | lower}}</p>
    <p>Match: {{.Custom.Quote.MatchScore}}%</p>
    <a href="{{.Custom.Author.URL}}">{{.Custom.Author.Name}}</a>

Example 2: Product page with a list of features

  preview.json:
    {
      "Product": {
        "Name": "Widget Pro",
        "Price": "49.99",
        "InStock": true,
        "Features": ["Fast setup", "API access", "Custom themes"]
      },
      "Company": {
        "Name": "Acme Corp"
      }
    }

  template:
    <h1>{{.Custom.Product.Name}} - {{.Custom.Product.Price}}</h1>
    {{if .Custom.Product.InStock}}<span class="badge">In Stock</span>{{end}}
    <ul>
      {{range .Custom.Product.Features}}
        <li>{{.}}</li>
      {{end}}
    </ul>

Example 3: Blog with tags and nested objects

  preview.json:
    {
      "Post": {
        "Title": "Getting Started",
        "ReadingTime": 5,
        "Tags": ["tutorial", "beginner"],
        "Author": {
          "Name": "Jane",
          "Avatar": "/img/jane.jpg"
        }
      }
    }

  template:
    <h1>{{.Custom.Post.Title}}</h1>
    <p>{{.Custom.Post.ReadingTime}} min read</p>
    {{with .Custom.Post.Author}}
      <img src="{{.Avatar}}" alt="{{.Name}}">
      <span>By {{.Name}}</span>
    {{end}}
    <div class="tags">
      {{range .Custom.Post.Tags}}
        <span class="tag">{{.}}</span>
      {{end}}
    </div>

Standard keys (Article, Translation, Site) in preview_data stay at
the top level. Everything else is automatically nested under Custom.

BASIC SYNTAX
------------

  Output a value:           {{.Article.Title}}
  Raw HTML (no escaping):   {{.Article.Content | safeHTML}}
  Comments:                 {{/* this is a comment */}}
  Variables:                {{$title := .Article.Title}}{{$title}}
  Sub-template:             {{template "header.html" .}}

CONDITIONALS
------------

  Simple if:
    {{if .Article.Subtitle}}
      <p>{{.Article.Subtitle}}</p>
    {{end}}

  If/else:
    {{if .Custom.Product.InStock}}
      <button>Buy Now</button>
    {{else}}
      <button disabled>Out of Stock</button>
    {{end}}

  With block (nil-safe access to nested objects):
    {{with .Custom.Post.Author}}
      <span>By {{.Name}}</span>    {{/* .Name refers to Post.Author.Name */}}
    {{else}}
      <span>Unknown author</span>
    {{end}}

  Comparison (eq, ne, lt, gt, le, ge):
    {{if gt .Custom.Product.Price 100}}
      <span class="premium">Premium</span>
    {{end}}

  Boolean AND/OR:
    {{if and .Custom.Product.InStock (gt .Custom.Product.Price 0)}}
      <button>Add to Cart</button>
    {{end}}

  NOT:
    {{if not .Article.Subtitle}}
      <p>No subtitle available</p>
    {{end}}

LOOPS (range)
-------------

  Loop over a list of strings:
    Data: {"Tags": ["news", "tech", "go"]}

    {{range .Tags}}
      <span class="tag">{{.}}</span>
    {{end}}

    Output: <span class="tag">news</span>
            <span class="tag">tech</span>
            <span class="tag">go</span>

  Loop with index:
    {{range $i, $tag := .Tags}}
      <span>{{$i}}: {{$tag}}</span>
    {{end}}

    Output: <span>0: news</span>
            <span>1: tech</span>
            <span>2: go</span>

  Loop over objects:
    Data: {"Custom": {"Team": [
            {"Name": "Alice", "Role": "Lead"},
            {"Name": "Bob", "Role": "Dev"}
          ]}}

    {{range .Custom.Team}}
      <div>{{.Name}} — {{.Role}}</div>
    {{end}}

    Output: <div>Alice — Lead</div>
            <div>Bob — Dev</div>

  Empty list fallback:
    {{range .Custom.Products}}
      <div>{{.Name}}</div>
    {{else}}
      <p>No products found.</p>
    {{end}}

  Loop with separator (no trailing comma):
    {{range $i, $tag := .Tags}}{{if $i}}, {{end}}{{$tag}}{{end}}

    Output: news, tech, go

AVAILABLE FUNCTIONS
-------------------

  safeHTML      Mark string as safe HTML (skip auto-escaping)
                {{.Translation.ContentHTML | safeHTML}}

  markdownToHTML  Convert markdown to rendered HTML
                {{.Article.Content | markdownToHTML}}

  formatDate    Format a date string. Default layout: "2006-01-02"
                {{.Article.PublishedAt | formatDate}}
                {{formatDate .Article.PublishedAt "Jan 2, 2006"}}

  truncate      Truncate string to N characters (adds "...")
                {{.Article.Title | truncate 50}}

  lower         Convert to lowercase
                {{.Article.Title | lower}}

  upper         Convert to uppercase
                {{.Site.Name | upper}}

  default       Use fallback value when field is nil or empty
                {{.Article.Subtitle | default "No subtitle"}}

  replace       Replace all occurrences of a substring
                {{replace .Article.Content "<br>" "<br/>"}}

  contains      Check if string contains substring (returns bool)
                {{if contains .Article.Keywords "news"}}...{{end}}

  join          Join a list with separator
                {{join .Tags ", "}}

  split         Split string by separator
                {{split .Article.Keywords ","}}

  map           Create a key-value map (for sub-templates)
                {{template "card.html" (map "title" .Article.Title "date" .Article.PublishedAt)}}

  list          Create a list from values
                {{$items := list "one" "two" "three"}}

HANDLING MISSING DATA
---------------------

Go templates render missing fields as empty (blank) by default. To show
fallback text when a field might be missing:

  Top-level default:
    {{.Article.Subtitle | default "Untitled"}}

  Nil-safe nested access (when parent might be nil):
    {{with .Custom.Quote}}
      <h1>{{.Text}}</h1>
      <p>Theme: {{.Theme}}</p>
    {{else}}
      <p>No quote available</p>
    {{end}}

  Conditional display:
    {{if .Article.Subtitle}}
      <p class="subtitle">{{.Article.Subtitle}}</p>
    {{end}}

PREVIEW DATA FILE (preview_data)
---------------------------------

Create a JSON file matching your template's expected structure:

  {
    "Quote": {
      "Text": "Sample quote text",
      "Theme": "inspiration",
      "MatchScore": 85
    },
    "Author": {
      "Name": "Jane Doe"
    },
    "Article": {
      "Title": "Sample Article",
      "PublishedAt": "2025-01-15T10:00:00Z"
    }
  }

Reference it in taufinity.yaml:

  preview_data: test-data/preview.json

DEBUGGING
---------

  TAUFINITY_DEBUG=1 taufinity template preview

Debug mode shows all available template tags and their current values,
so you can see exactly what data is available to your template.

EXAMPLES
--------

Minimal article template:

  <!DOCTYPE html>
  <html>
  <head><title>{{.Article.Title}}</title></head>
  <body>
    <h1>{{.Article.Title}}</h1>
    {{if .Article.Subtitle}}<p>{{.Article.Subtitle}}</p>{{end}}
    <article>{{.Translation.ContentHTML | safeHTML}}</article>
    <footer>Published: {{.Article.PublishedAt | formatDate}}</footer>
  </body>
  </html>

Custom schema template (e.g. quotes):

  <h1>{{.Custom.Quote.Text}}</h1>
  <p>Theme: {{.Custom.Quote.Theme | lower}}</p>
  {{with .Custom.Author}}<p>By: {{.Name}}</p>{{end}}

Multi-template with partials:

  {{/* header.html */}}
  <header><h1>{{.Title}}</h1></header>

  {{/* page.html (entry template) */}}
  {{template "header.html" (map "Title" .Article.Title)}}
  <main>{{.Translation.ContentHTML | safeHTML}}</main>

`)
}
