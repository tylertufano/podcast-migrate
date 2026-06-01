package cmd

import "github.com/spf13/cobra"

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:     "podcast-migrate",
		Version: version,
		Short:   "Migrate podcast subscriptions and play state between apps",
		Long: `podcast-migrate moves subscriptions and episode play state between
podcast applications (Apple Podcasts, Overcast, and more).

Run 'podcast-migrate help <command>' for details on a specific command.`,
	}

	root.AddCommand(
		migrateCmd(),
		markPlayedCmd(),
		exportCmd(),
		importCmd(),
		observeCmd(),
	)
	return root
}
