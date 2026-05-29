package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/tyler/podcast-migrate/internal/overcast"
)

func markPlayedCmd() *cobra.Command {
	var (
		episodeURL   string
		overcastEmail    string
		overcastPassword string
		requestDelay time.Duration
	)

	cmd := &cobra.Command{
		Use:   "mark-played",
		Short: "Mark a specific Overcast episode as played via the web API",
		Example: `  # Mark a single episode as played using its overcast.fm URL
  podcast-migrate mark-played --url https://overcast.fm/+pGPCM1nmo
  # Credentials via env vars:
  #   OVERCAST_EMAIL=you@example.com OVERCAST_PASSWORD=secret podcast-migrate mark-played ...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if overcastEmail == "" {
				overcastEmail = os.Getenv("OVERCAST_EMAIL")
			}
			if overcastPassword == "" {
				overcastPassword = os.Getenv("OVERCAST_PASSWORD")
			}
			if overcastEmail == "" {
				return fmt.Errorf("Overcast credentials required: set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email / --overcast-password")
			}
			if episodeURL == "" {
				return fmt.Errorf("--url is required (e.g. https://overcast.fm/+pGPCM1nmo)")
			}

			if requestDelay <= 0 {
				requestDelay = overcast.DefaultRequestDelay
			}

			ctx := context.Background()

			fmt.Printf("overcast: authenticating as %s...\n", overcastEmail)
			client, err := overcast.Login(ctx, overcastEmail, overcastPassword)
			if err != nil {
				return fmt.Errorf("authentication failed: %w", err)
			}
			fmt.Println("overcast: authenticated.")

			fmt.Printf("overcast: fetching numeric ID for %s...\n", episodeURL)
			time.Sleep(requestDelay)
			numericID, err := overcast.FetchEpisodeNumericID(ctx, client, episodeURL)
			if err != nil {
				return fmt.Errorf("fetch episode ID: %w", err)
			}
			fmt.Printf("overcast: numeric ID: %s\n", numericID)

			fmt.Printf("overcast: marking episode as played (p=%d)...\n", overcast.PlayedSentinel)
			time.Sleep(requestDelay)
			if err := overcast.SetProgress(ctx, client, numericID, overcast.PlayedSentinel); err != nil {
				return fmt.Errorf("set_progress: %w", err)
			}

			fmt.Printf("overcast: ✓ episode marked as played.\n")
			return nil
		},
	}

	cmd.Flags().StringVar(&episodeURL, "url", "", "Overcast episode URL, e.g. https://overcast.fm/+pGPCM1nmo (required)")
	cmd.Flags().StringVar(&overcastEmail, "overcast-email", "", "Overcast account email (or OVERCAST_EMAIL env var)")
	cmd.Flags().StringVar(&overcastPassword, "overcast-password", "", "Overcast account password (or OVERCAST_PASSWORD env var)")
	cmd.Flags().DurationVar(&requestDelay, "request-delay", overcast.DefaultRequestDelay, "delay between API requests")

	_ = cmd.MarkFlagRequired("url")

	return cmd
}
