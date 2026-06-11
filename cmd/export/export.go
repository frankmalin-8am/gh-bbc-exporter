package export

import (
	"errors"
	"fmt"

	"github.com/katiem0/gh-bbc-exporter/internal/data"
	"github.com/katiem0/gh-bbc-exporter/internal/log"
	"github.com/katiem0/gh-bbc-exporter/internal/utils"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func NewCmdExport() *cobra.Command {
	cmdExportFlags := data.CmdExportFlags{}

	exportCmd := &cobra.Command{
		Use:   "export [flags]",
		Short: "Export repository and metadata from Bitbucket Cloud",
		Long: `Export repository and metadata from Bitbucket Cloud for GitHub Cloud import.

Pull requests whose commit SHAs can no longer be resolved (e.g. objects GC'd
after branch deletion) would otherwise be silently dropped by the GitHub
Enterprise Importer (GEI).  The --sha-fallback flag controls how these are
handled:

  none     Pass the unresolvable SHA through as-is.  GEI will silently drop
           any PR it cannot anchor to a commit.

  related  Try the merge-commit SHA first (MERGED PRs only), then fall back
           to the base-branch SHA.  Both must be full 40-character SHAs;
           short/unresolvable values are not used as substitutes.

  nearest  (default) All of 'related', plus a final fallback: find the most
           recent commit on the destination branch that predates the PR's open
           date using the locally cloned repository.  Historically approximate
           but always produces a valid SHA that GEI will accept.`,
		PreRunE: func(exportCmd *cobra.Command, args []string) error {
			if len(cmdExportFlags.Workspace) == 0 {
				return errors.New("a bitbucket workspace must be specified")
			}
			if len(cmdExportFlags.Repository) == 0 {
				return errors.New("a bitbucket repository must be specified")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			logger, err := log.NewLogger(cmdExportFlags.Debug)
			if err != nil {
				return fmt.Errorf("failed to initialize logger: %w", err)
			}
			defer func() {
				_ = logger.Sync()
			}()
			zap.ReplaceGlobals(logger)
			return runCmdExport(&cmdExportFlags, logger)
		},
	}

	exportCmd.Flags().SortFlags = false
	exportCmd.PersistentFlags().SortFlags = false

	utils.SetupCommandUsageTemplate(exportCmd, 100)

	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.BitbucketAPIURL, "bbc-api-url", "a",
		"https://api.bitbucket.org/2.0", "Bitbucket API to use")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.BitbucketAccessToken, "access-token", "t", "",
		"Bitbucket workspace access token for authentication (env: BITBUCKET_ACCESS_TOKEN)")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.BitbucketAPIToken, "api-token", "", "",
		"Bitbucket API token for authentication (env: BITBUCKET_API_TOKEN)")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.BitbucketEmail, "email", "e", "",
		"Atlassian account email for API token authentication (env: BITBUCKET_EMAIL)")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.BitbucketUser, "user", "u", "",
		"Bitbucket username for basic authentication (env: BITBUCKET_USERNAME)")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.BitbucketAppPass, "app-password", "p", "",
		"Bitbucket app password for basic authentication (env: BITBUCKET_APP_PASSWORD)")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.Workspace, "workspace", "w", "",
		"Bitbucket workspace name")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.Repository, "repo", "r", "",
		"Name of the repository to export from Bitbucket Cloud")
	exportCmd.PersistentFlags().StringVar(&cmdExportFlags.TempDir, "temp-dir", "",
		"Temporary directory for cloning (env: BITBUCKET_TEMP_DIR)")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.OutputDir, "output", "o", "",
		"Output directory for exported data (default: ./bitbucket-export-TIMESTAMP)")
	exportCmd.PersistentFlags().BoolVar(&cmdExportFlags.OpenPRsOnly, "open-prs-only", false,
		"Export only open pull requests and ignore closed/merged ones")
	exportCmd.PersistentFlags().StringVarP(&cmdExportFlags.PRsFromDate, "prs-from-date", "", "",
		"Export pull requests created on or after this date (format: YYYY-MM-DD)")
	exportCmd.PersistentFlags().BoolVar(&cmdExportFlags.SkipCommitLookup, "skip-commit-lookup", false,
		"Skip Bitbucket API lookups to retrieve commit SHAs (use local lookup only)")
	exportCmd.PersistentFlags().StringVar(&cmdExportFlags.SHAFallback, "sha-fallback", "nearest",
		"How to handle PRs with unresolvable commit SHAs: none (pass through, GEI will drop), related (use merge/base SHA), nearest (related + nearest local commit by date)")
	exportCmd.PersistentFlags().BoolVar(&cmdExportFlags.AllowAmbiguousRefs, "allow-ambiguous-refs", false,
		"Warn instead of failing when a branch and tag share the same name (checkout behaviour will favour the branch)")
	exportCmd.PersistentFlags().BoolVarP(&cmdExportFlags.Debug, "debug", "d", false, "Enable debug logging")

	if err := exportCmd.MarkPersistentFlagRequired("workspace"); err != nil {
		fmt.Printf("Error marking workspace flag as required: %v\n", err)
	}
	if err := exportCmd.MarkPersistentFlagRequired("repo"); err != nil {
		fmt.Printf("Error marking repository flag as required: %v\n", err)
	}
	return exportCmd
}

func runCmdExport(cmdExportFlags *data.CmdExportFlags, logger *zap.Logger) error {
	logger.Info("Starting Bitbucket Cloud export",
		zap.String("workspace", cmdExportFlags.Workspace),
		zap.String("repository", cmdExportFlags.Repository))

	// Read environment variables
	utils.SetupEnvironmentCredentials(cmdExportFlags)

	// Validate inputs
	if err := utils.ValidateExportFlags(cmdExportFlags); err != nil {
		return err
	}

	if cmdExportFlags.BitbucketAccessToken != "" {
		logger.Info("Using workspace access token authentication")
	} else if cmdExportFlags.BitbucketAPIToken != "" {
		logger.Info("Using API token authentication")
	} else if cmdExportFlags.BitbucketUser != "" && cmdExportFlags.BitbucketAppPass != "" {
		logger.Info("Using basic authentication",
			zap.String("username", cmdExportFlags.BitbucketUser))
	}

	client := utils.NewClient(
		cmdExportFlags.BitbucketAPIURL,
		cmdExportFlags.BitbucketAccessToken,
		cmdExportFlags.BitbucketAPIToken,
		cmdExportFlags.BitbucketEmail,
		cmdExportFlags.BitbucketUser,
		cmdExportFlags.BitbucketAppPass,
		logger,
		cmdExportFlags.OutputDir,
		cmdExportFlags.SkipCommitLookup,
		cmdExportFlags.SHAFallback,
	)

	if cmdExportFlags.OpenPRsOnly {
		logger.Info("Filtering: Only open PRs will be exported")
	}
	if cmdExportFlags.PRsFromDate != "" {
		logger.Info("Filtering: PRs from date", zap.String("from_date", cmdExportFlags.PRsFromDate))
	}

	// Apply commit SHA expansion behavior
	if cmdExportFlags.SkipCommitLookup {
		logger.Info("Skipping Bitbucket API commit SHA lookups (will look locally only)")
	}
	logger.Info("SHA fallback mode", zap.String("sha_fallback", cmdExportFlags.SHAFallback))

	exporter := utils.NewExporter(client, cmdExportFlags.OutputDir, logger, cmdExportFlags.OpenPRsOnly, cmdExportFlags.PRsFromDate)
	if cmdExportFlags.AllowAmbiguousRefs {
		exporter.SetAllowAmbiguousRefs(true)
		logger.Info("Ambiguous ref check: branch/tag name collisions will be warned, not failed (--allow-ambiguous-refs)")
	}

	if cmdExportFlags.TempDir != "" {
		exporter.SetTempDir(cmdExportFlags.TempDir)
		logger.Debug("Using custom temporary directory", zap.String("temp_dir", cmdExportFlags.TempDir))
	}
	// Run export
	if err := exporter.Export(cmdExportFlags.Workspace, cmdExportFlags.Repository); err != nil {
		logger.Error("Export failed")
		return err
	}

	// Print success message
	outputPath := exporter.GetOutputPath()
	utils.PrintSuccessMessage(outputPath)

	logger.Info("Export completed successfully")
	return nil
}
