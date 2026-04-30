package cli

import (
	"encoding/json"
	"fmt"
	stdruntime "runtime"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/clement-tourriere/debux/internal/version"
	"github.com/spf13/cobra"
)

type versionInfo struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	Date         string `json:"date"`
	GoOS         string `json:"goos"`
	GoArch       string `json:"goarch"`
	DefaultImage string `json:"defaultImage"`
}

func newVersionCmd() *cobra.Command {
	var outputJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show debux version information",
		Example: `  debux version
  debux version --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := versionInfo{
				Version:      version.String(),
				Commit:       version.Commit,
				Date:         version.Date,
				GoOS:         stdruntime.GOOS,
				GoArch:       stdruntime.GOARCH,
				DefaultImage: runtime.DefaultImage,
			}
			if outputJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "debux %s\ncommit: %s\nbuilt: %s\ndefault image: %s\n", info.Version, info.Commit, info.Date, info.DefaultImage)
			return nil
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "json", false, "Print version information as JSON")
	return cmd
}
