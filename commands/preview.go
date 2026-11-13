package commands

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/taufinity/cli/internal/api"
)

const bannerInitFile = "voorbeeld.csv"

var (
	flagBannerData string
	flagBannerRow  int
	bannerPort     int
	bannerNoOpen   bool
	bannerNoWatch  bool
	bannerInit     bool
	bannerForce    bool
)

var bannerPreviewCmd = &cobra.Command{
	Use:   "preview [map]",
	Short: "Bekijk een banner-sjabloon in de browser (ververst vanzelf)",
	Long: `Bekijk een BANNER-sjabloon: draai dit in de sjabloonmap, het ingevulde
resultaat opent in de browser en ververst zichzelf als je een bestand wijzigt.

Voor sjablonen van artikelen van een site is er 'taufinity template preview'.

MAP
  De map met het sjabloon (standaard de huidige map). De map wordt gezipt met
  dezelfde uitsluitingen als bij uploaden (.gitignore, .taufinityignore).

GEGEVENSBESTAND
  --data <csv>   gebruik dit bestand
  zonder --data  het enige *.csv in de map of in data/ wordt gebruikt; zijn er
                 meer, dan noemt het commando ze en vraagt om --data
  geen CSV       de server maakt zelf voorbeeldregels (voorbeeld, leeg, lang)
  De scheiding (komma of puntkomma) wordt uit de kopregel gehaald, UTF-8 met of
  zonder BOM. De kopregel zijn de veldnamen. Maximaal 50 regels.

REGEL
  --row <n>      toon alleen regel n, genummerd zoals in de Product Layer: de
                 kopregel is regel 1, de eerste gegevensregel is regel 2.

VOORBEELDBESTAND
  --init         schrijf voorbeeld.csv naast het sjabloon: de kop met alle
                 velden en een regel met de waarden van de voorbeeldregel van de server. Velden die
                 eindigen op _url of _file blijven leeg. Overschrijft niets zonder --force.

VERVERSEN
  Standaard wordt de map en het gegevensbestand bewaakt: na 250 ms rust wordt
  opnieuw ingevuld en herlaadt de pagina, met dezelfde regel en weergave. Een
  fout laat de laatste goede banner staan onder een melding. --no-watch zet dit
  uit. Ctrl-C stopt en ruimt de tijdelijke mappen op.

PRIJS
  1 credit per ontwikkelaar, per sjabloon, per uur bekijken. De sessie loopt 60
  minuten vanaf de eerste keer invullen; opslaan daarna in dat uur is gratis.
  Na het invullen toont de terminal "1 credit voor deze template, sessie tot
  HH:MM" of "binnen de sessie tot HH:MM, geen extra credit" (lokale tijd), en
  de pagina toont dezelfde tekst. De regel verschijnt alleen als hij verandert.

  De knop "Echte render (normale prijs)" op de pagina vraagt Studio om een echte
  render van de regel die je ziet, tegen de normale prijs: PNG 1 credit, GIF van
  10 frames 2 credits. De pagina vraagt dit één keer in de browser en onthoudt
  daarna je bevestiging.

SLEUTEL
  Bij het eerste gebruik maakt dit commando .taufinity/preview.json in de
  sjabloonmap, met een willekeurige template_key. Laat dit bestand staan,
  dan blijft de sleutel gelijk; er staat geen geheim in. De map .taufinity/
  gaat nooit mee in de zip en heeft een eigen .gitignore, dus ook niet in git.
  De sleutel hoort bij deze map: het bestand onthoudt welke map (als code, geen
  pad of gebruikersnaam). Een gekopieerde of verplaatste map krijgt daarom een
  eigen sleutel, en daarmee een eigen sessie van 1 credit.

VEILIGHEID
  Alleen gewone bestanden in de map gaan mee: symbolische links worden overgeslagen
  (met een melding). Geheimen (.env, credentials*.json, .npmrc, .netrc, SSH-sleutels,
  .aws/, *.tfvars, ...) en alles uit de .gitignore-bestanden tot de git-root blijven
  eruit. Grenzen: 5.000 bestanden en 32 MB ingepakt omhoog, 64 MB, 5.000 bestanden
  en 60 regels terug, en hooguit 2.000 bewaakte mappen. Een sjabloon wordt nooit
  vanzelf opnieuw verstuurd na een fout of time-out; na een 429 of 503 pauzeert
  het verversen tot Retry-After. De banner draait op een eigen poort in een
  sandbox, los van de pagina.

OPSLAG
  Studio bewaart het sjabloon zelf nooit: het leeft alleen in het geheugen
  voor de duur van één verzoek.

HOE HET WERKT
  Het invullen gebeurt op de server met dezelfde code als bij een echte render.
  Wat je ziet is dus precies wat een render krijgt, minus de capture (timing,
  frames, compressie en de controles). De proefrender van een batch blijft de
  echte controle. De pagina luistert alleen op 127.0.0.1.`,
	Example: `  taufinity preview
  taufinity preview --init
  taufinity preview ./banners/klant-x --row 3`,
	Args: cobra.MaximumNArgs(1),
	RunE: runBannerPreview,
}

func init() {
	rootCmd.AddCommand(bannerPreviewCmd)
	f := bannerPreviewCmd.Flags()
	f.StringVar(&flagBannerData, "data", "", "CSV-bestand met gegevens (standaard het enige *.csv in de map of in data/)")
	f.IntVar(&flagBannerRow, "row", 0, "toon alleen deze regel (kopregel = 1, eerste gegevensregel = 2)")
	f.IntVar(&bannerPort, "port", 0, "poort voor de lokale pagina (standaard een vrije poort)")
	f.BoolVar(&bannerNoOpen, "no-open", false, "open de browser niet, toon alleen het adres")
	f.BoolVar(&bannerNoWatch, "no-watch", false, "bewaak de map niet, vul één keer in")
	f.BoolVar(&bannerInit, "init", false, "schrijf voorbeeld.csv naast het sjabloon")
	f.BoolVar(&bannerForce, "force", false, "overschrijf een bestaand voorbeeld.csv (met --init)")
}

// bannerRenderClientTimeout covers the render endpoint's own wait plus the upload and
// the download around it.
const bannerRenderClientTimeout = renderTimeout + 30*time.Second

// newBannerRenderClient is the client of the "Echte render" button: a paid call,
// so it is never retried, and it waits for a slow render.
func newBannerRenderClient() *api.Client {
	client := api.NewRender(GetAPIURL(), bannerRenderClientTimeout)
	client.SetDebug(IsDebug())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}
	return client
}

func newBannerClient() *api.Client {
	client := api.NewPreview(GetAPIURL())
	client.SetDebug(IsDebug())
	if org := GetOrg(); org != "" {
		client.SetOrg(org)
	}
	return client
}

func runBannerPreview(cmd *cobra.Command, args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is geen map", dir)
	}
	if err := checkTempOutsideTemplate(dir); err != nil {
		return err
	}
	cleanStaleBannerDirs(os.TempDir(), bannerStaleAfter)
	src := &bannerSource{Client: newBannerClient(), Dir: dir, DataFlag: flagBannerData, Row: flagBannerRow}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if bannerInit {
		return runBannerInit(ctx, src, bannerForce)
	}

	_, dataPath, err := loadBannerRows(dir, flagBannerData, flagBannerRow)
	if err != nil {
		return err
	}
	if dataPath != "" {
		Print("Gegevens: %s\n", dataPath)
	}
	Print("Sjabloon invullen op de server...\n")
	res, err := src.Fill(ctx)
	if err != nil {
		return err
	}
	srv := newBannerServer()
	srv.Renderer = &bannerRenderer{Client: newBannerRenderClient(), Dir: dir, DataFlag: flagBannerData, Row: flagBannerRow}
	defer srv.Close()
	ln, err := srv.Listen(bannerPort)
	if err != nil {
		os.RemoveAll(res.Dir)
		return err
	}
	srv.Swap(res)
	go func() { _ = srv.Serve(ln) }()

	Print("Voorbeeld op %s\n", srv.URL())
	if !bannerNoOpen {
		if err := browser.OpenURL(srv.URL()); err != nil {
			Print("De browser kon niet worden geopend; open het adres zelf.\n")
		}
	}
	if bannerNoWatch {
		Print("Ctrl-C om te stoppen.\n")
		<-ctx.Done()
		Print("Gestopt.\n")
		return nil
	}
	sess := &bannerSession{Fill: src.Fill, Srv: srv, Warn: func(m string) { fmt.Fprintln(os.Stderr, m) }}
	Print("Wijzigingen worden bewaakt; Ctrl-C om te stoppen.\n")
	err = watchBanner(ctx, dir, dataPath, bannerDebounce, latestOnly(ctx, func(c context.Context) {
		if sess.Refresh(c) == nil {
			Print("Ververst.\n")
		}
	}))
	Print("Gestopt.\n")
	return err
}

// bannerInitCSV is the content of voorbeeld.csv: semicolon separated, the
// header with every field, one row with the values the server generated for the
// "voorbeeld" row, copied as they are (a missing or empty value stays empty). Fields ending in _url or
// _file stay empty: a generated placeholder image is no use in a sheet.
func bannerInitCSV(rep *bannerReport) ([]byte, error) {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	w.Comma = ';'
	var sample map[string]string
	for _, r := range rep.Rows {
		if r.Label == "voorbeeld" {
			sample = r.Values
			break
		}
	}
	header := make([]string, len(rep.Fields))
	vals := make([]string, len(rep.Fields))
	for i, f := range rep.Fields {
		name := string(f)
		header[i] = name
		switch {
		case strings.HasSuffix(name, "_url"), strings.HasSuffix(name, "_file"):
			vals[i] = ""
		default:
			vals[i] = sample[name]
		}
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}
	if err := w.Write(vals); err != nil {
		return nil, err
	}
	w.Flush()
	return []byte(sb.String()), w.Error()
}

func runBannerInit(ctx context.Context, src *bannerSource, force bool) error {
	target := filepath.Join(src.Dir, bannerInitFile)
	if _, err := os.Stat(target); err == nil && !force {
		return fmt.Errorf("%s bestaat al; gebruik --force om het te overschrijven", bannerInitFile)
	}
	src.NoRows = true
	res, err := src.Fill(ctx)
	if err != nil {
		return err
	}
	defer os.RemoveAll(res.Dir)
	out, err := bannerInitCSV(res.Report)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, out, 0o644); err != nil {
		return err
	}
	Print("%s geschreven met %d velden. Vul de gegevens in en draai 'taufinity preview'.\n", target, len(res.Report.Fields))
	return nil
}
