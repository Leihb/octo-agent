package mswe

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

// HarnessConfig is the config.json passed to
// `python -m multi_swe_bench.harness.run_evaluation --config <file>`.
//
// The field set mirrors the harness's CliArgs dataclass (verified against
// multi_swe_bench 1.1.2). It's loaded via dataclass_json/marshmallow, which
// rejects unknown keys and requires every non-defaulted field — so we emit the
// full set explicitly. Optional fields we don't use (specifics, skips,
// global_env) are nil → JSON null.
type HarnessConfig struct {
	Workdir               string   `json:"workdir"`
	PatchFiles            []string `json:"patch_files"`
	DatasetFiles          []string `json:"dataset_files"`
	ForceBuild            bool     `json:"force_build"`
	OutputDir             string   `json:"output_dir"`
	Specifics             []string `json:"specifics"`
	Skips                 []string `json:"skips"`
	RepoDir               string   `json:"repo_dir"`
	NeedClone             bool     `json:"need_clone"`
	GlobalEnv             []string `json:"global_env"`
	ClearEnv              bool     `json:"clear_env"`
	StopOnError           bool     `json:"stop_on_error"`
	MaxWorkers            int      `json:"max_workers"`
	MaxWorkersBuildImage  int      `json:"max_workers_build_image"`
	MaxWorkersRunInstance int      `json:"max_workers_run_instance"`
	FixPatchRunCmd        string   `json:"fix_patch_run_cmd"`
	LogDir                string   `json:"log_dir"`
	LogLevel              string   `json:"log_level"`
	LogToConsole          bool     `json:"log_to_console"`
	HumanMode             bool     `json:"human_mode"`
}

// NewHarnessConfig builds a config with conservative defaults for a small first
// run: the harness clones repos itself (need_clone) into repo_dir, builds and
// runs serially (workers=1) so progress is easy to follow, and does not abort
// the whole batch on a single instance error (stop_on_error=false).
func NewHarnessConfig(workdir, datasetFile, predictionsFile, outputDir string) HarnessConfig {
	return HarnessConfig{
		Workdir:               workdir,
		PatchFiles:            []string{predictionsFile},
		DatasetFiles:          []string{datasetFile},
		ForceBuild:            false,
		OutputDir:             outputDir,
		Specifics:             nil,
		Skips:                 nil,
		RepoDir:               filepath.Join(workdir, "repos"),
		NeedClone:             true,
		GlobalEnv:             nil,
		ClearEnv:              true,
		StopOnError:           false,
		MaxWorkers:            1,
		MaxWorkersBuildImage:  1,
		MaxWorkersRunInstance: 1,
		FixPatchRunCmd:        "",
		LogDir:                filepath.Join(outputDir, "logs"),
		LogLevel:              "INFO",
		LogToConsole:          true,
		HumanMode:             true,
	}
}

// Write emits the config as indented JSON.
func (c HarnessConfig) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("mswe: write harness config: %w", err)
	}
	return nil
}
