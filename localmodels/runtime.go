package localmodels

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/daltoniam/overload/catalog"
)

// LlamaVersion is an installed llama.cpp build.
type LlamaVersion struct {
	Build  int
	Commit string
}

var llamaVersionLine = regexp.MustCompile(`(?m)^version: (\d+) \(([^)\s]*)\)`)

// ParseLlamaVersion reads `llama-server --version` output.
func ParseLlamaVersion(output string) (LlamaVersion, error) {
	match := llamaVersionLine.FindStringSubmatch(output)
	if match == nil {
		return LlamaVersion{}, errors.New("unrecognized llama.cpp version output")
	}
	build, err := strconv.Atoi(match[1])
	if err != nil {
		return LlamaVersion{}, err
	}
	return LlamaVersion{Build: build, Commit: match[2]}, nil
}

// DetectLlama runs binary --version.
func DetectLlama(ctx context.Context, binary string) (LlamaVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if ctx.Err() != nil {
		return LlamaVersion{}, fmt.Errorf("%s --version: %w", binary, ctx.Err())
	}
	version, parseErr := ParseLlamaVersion(string(output))
	switch {
	case parseErr == nil:
		// Some builds print the version and exit non-zero; the version
		// line is what matters.
		return version, nil
	case err != nil:
		return LlamaVersion{}, fmt.Errorf("%s --version: %w", binary, err)
	default:
		return LlamaVersion{}, parseErr
	}
}

// CheckRuntime reports whether version can load model, with the fix when
// it cannot.
func CheckRuntime(model catalog.Model, version LlamaVersion) error {
	if model.Runtime == nil || model.Runtime.Engine != "llama.cpp" {
		return fmt.Errorf("%s does not run on llama.cpp", model.ID)
	}
	if model.Runtime.MinBuild > 0 && version.Build == 0 {
		return fmt.Errorf("%s needs llama.cpp build b%d or newer, but this llama-server does not report a build number (built from source without git?); pass --skip-version-check if you know it is new enough", model.ID, model.Runtime.MinBuild)
	}
	if model.Runtime.MinBuild > 0 && version.Build < model.Runtime.MinBuild {
		return fmt.Errorf("%s needs llama.cpp build b%d or newer, but llama-server is b%d; install a newer prebuilt release from https://github.com/ggml-org/llama.cpp/releases (Homebrew's formula can lag new architectures by weeks)", model.ID, model.Runtime.MinBuild, version.Build)
	}
	return nil
}
