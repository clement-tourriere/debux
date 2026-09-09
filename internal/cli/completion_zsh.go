package cli

import (
	"bytes"
	"strings"

	"github.com/spf13/cobra"
)

func genZshCompletionWithSubstringMatching(root *cobra.Command, cmd *cobra.Command) error {
	var buf bytes.Buffer
	if err := root.GenZshCompletion(&buf); err != nil {
		return err
	}
	_, err := cmd.OutOrStdout().Write([]byte(patchZshCompletionForSubstringMatches(buf.String())))
	return err
}

func patchZshCompletionForSubstringMatches(script string) string {
	patched, ok := tryPatchZshCompletionForSubstringMatches(script)
	if !ok {
		// A cobra upgrade moved the anchors this patch targets. Serving the
		// stock script keeps every completion working with normal prefix
		// matching; a half-applied patch would break ALL dynamic zsh
		// completions silently.
		return script
	}
	return patched
}

func tryPatchZshCompletionForSubstringMatches(script string) (string, bool) {
	// Cobra's stock zsh script feeds candidates to _describe. _describe asks zsh
	// to apply normal prefix matching, so it hides intentional pod substring
	// matches such as `k8s://inter` -> `k8s://webapp-internal-api-...`.
	// Capture raw values/descriptions and use compadd -U instead. The Go
	// completion code remains responsible for filtering candidates.
	var ok bool
	script, ok = replaceExactlyOnce(script,
		"local -a completions",
		"local -a completions completionValues completionDescriptions",
	)
	if !ok {
		return "", false
	}
	script, ok = replaceExactlyOnce(script,
		`            # If requested, completions are returned with a description.
            # The description is preceded by a TAB character.
            # For zsh's _describe, we need to use a : instead of a TAB.
            # We first need to escape any : as part of the completion itself.
            comp=${comp//:/\\:}

            local tab="$(printf '\t')"
            comp=${comp//$tab/:}

            __debux_debug "Adding completion: ${comp}"
            completions+=${comp}
            lastComp=$comp`,
		`            # If requested, completions are returned with a description.
            # The description is preceded by a TAB character.
            local tab="$(printf '\t')"
            local value="${comp%%$tab*}"
            local desc=""
            if [[ "$comp" == *$tab* ]]; then
                desc="${comp#*$tab}"
            fi
            completionValues+=("${value}")
            completionDescriptions+=("${desc}")

            # Keep Cobra's original _describe-compatible array around for
            # fallback paths and debug output.
            comp=${comp//:/\\:}
            comp=${comp//$tab/:}

            __debux_debug "Adding completion: ${comp}"
            completions+=${comp}
            lastComp=$comp`,
	)
	if !ok {
		return "", false
	}
	script, ok = replaceExactlyOnce(script,
		"        __debux_debug \"Calling _describe\"\n        if eval _describe $keepOrder \"completions\" completions $flagPrefix $noSpace; then",
		`        __debux_debug "Calling compadd for dynamic completions"
        local -a completionDisplayValues
        local completionIndex
        for (( completionIndex=1; completionIndex<=${#completionValues}; completionIndex++ )); do
            if [ -n "${completionDescriptions[$completionIndex]}" ]; then
                completionDisplayValues+=("${completionValues[$completionIndex]}  -- ${completionDescriptions[$completionIndex]}")
            else
                completionDisplayValues+=("${completionValues[$completionIndex]}")
            fi
        done
        if [ ${#completionValues} -ne 0 ] && eval compadd -U -V completions -d completionDisplayValues $flagPrefix $noSpace -a completionValues; then`,
	)
	if !ok {
		return "", false
	}
	script = strings.ReplaceAll(script, `__debux_debug "_describe found some completions"`, `__debux_debug "compadd found some completions"`)
	script = strings.ReplaceAll(script, `__debux_debug "_describe did not find completions."`, `__debux_debug "compadd did not find completions."`)
	return script, true
}

// replaceExactlyOnce replaces old with new and reports whether the anchor was
// actually found, so template drift in a dependency fails loudly instead of
// producing a half-patched script.
func replaceExactlyOnce(s, old, new string) (string, bool) {
	if !strings.Contains(s, old) {
		return s, false
	}
	return strings.Replace(s, old, new, 1), true
}
