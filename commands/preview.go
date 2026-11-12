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
                 velden en een regel met ‹naam›-plaatshouders. Velden die
                 eindigen op _url of _file blijven leeg. Overschrijft niets zonder --force.

VERVERSEN
  Standaard wordt de map en het gegevensbestand bewaakt: na 250 ms rust wordt
  opnieuw ingevuld en herlaadt de pagina, met dezelfde regel en weergave. Een
  fout laat de laatste goede banner staan onder een melding. --no-watch zet dit
  uit. Ctrl-C stopt en ruimt de tijdelijke mappen op.

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

func newBannerClient() *api.Client {
	client := api.NewNoRateLimitRetry(GetAPIURL())
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
	err = watchBanner(ctx, dir, dataPath, bannerDebounce, func() {
		if sess.Refresh(ctx) == nil {
			Print("Ververst.\n")
		}
	})
	Print("Gestopt.\n")
	return err
}

// bannerInitCSV is the content of voorbeeld.csv: semicolon separated, the
// header with every field, one row with the values the server generated for the
// "voorbeeld" row (defaults), ‹naam› placeholders where it sent none. Fields ending in _url or
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
			vals[i] = "‹" + name + "›"
			if v, ok := sample[name]; ok && v != "" {
				vals[i] = v
			}
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
