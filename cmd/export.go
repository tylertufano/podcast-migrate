package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func exportCmd() *cobra.Command {
	var (
		from               string
		out                string
		sqlitePath         string
		opmlFallback       string
		overcastSourceOPML string
		opmlSourceFile     string
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a provider's library to a portable JSON file",
		Example: `  podcast-migrate export --from podcasts --out ~/Desktop/my-podcasts.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := buildProvider(from, sqlitePath, opmlFallback, overcastSourceOPML, "", "", "", "", "", opmlSourceFile, "")
			if err != nil {
				return err
			}

			lib, err := p.GetLibrary(context.Background())
			if err != nil {
				return fmt.Errorf("export: %w", err)
			}

			data, err := json.MarshalIndent(lib, "", "  ")
			if err != nil {
				return fmt.Errorf("export: marshal: %w", err)
			}

			if out == "" || out == "-" {
				_, err = os.Stdout.Write(data)
				return err
			}

			if err := os.WriteFile(out, data, 0644); err != nil {
				return fmt.Errorf("export: write %s: %w", out, err)
			}
			fmt.Printf("exported %d podcasts, %d episode states → %s\n",
				len(lib.Podcasts), len(lib.Episodes), out)
			if lib.SkippedInternalPodcasts > 0 {
				fmt.Printf("note: skipped %d podcast(s) with Apple-internal feed URLs — no public RSS feed exists for these.\n",
					lib.SkippedInternalPodcasts)
			}
			if lib.PaywalledEpisodesIncluded > 0 {
				fmt.Printf("note: %d Apple Podcasts Subscription (PSUB/PLUS) episode states included —\n"+
					"      these use Apple-proprietary GUIDs; matching will fall back to podcast title + pub date.\n",
					lib.PaywalledEpisodesIncluded)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "source app: podcasts, overcast, opml (required)")
	cmd.Flags().StringVar(&out, "out", "-", "output path (default: stdout)")
	cmd.Flags().StringVar(&sqlitePath, "sqlite", "", "path to MTLibrary.sqlite")
	cmd.Flags().StringVar(&opmlFallback, "opml-fallback", "", "path to Apple Podcasts OPML export")
	cmd.Flags().StringVar(&overcastSourceOPML, "overcast-source-opml", "", "path to Overcast OPML export")
	cmd.Flags().StringVar(&opmlSourceFile, "opml-file", "",
		"path to source OPML file (required when --from opml)")
	_ = cmd.MarkFlagRequired("from")

	return cmd
}
