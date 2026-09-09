package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

const sessionOptionsLabel = "debux.clement-tourriere/options-hash"
const sessionOptionsEnv = "DEBUX_OPTIONS_HASH"

// sessionOptionsHash identifies creation-time options, never a shell command.
// Store only a digest, not potentially secret --env values, in metadata. Old
// sessions without this metadata are still attachable explicitly, but cannot
// silently satisfy a new exec request whose options we cannot verify.
func sessionOptionsHash(opts DebugOpts, identity ...string) string {
	profile := opts.Profile
	if profile == "" {
		profile = ProfileGeneral
	}
	caps := normalizeCapabilities(opts.CapAdd)
	slices.Sort(caps)
	caps = slices.Compact(caps)
	data, _ := json.Marshal(struct {
		Version                                   int
		Image, User, Profile, PullPolicy          string
		Privileged, ShareVolumes, ReadOnlyVolumes bool
		Env, Tools, CapAdd, Identity              []string
	}{1, opts.Image, opts.User, profile, strings.ToLower(opts.PullPolicy), opts.Privileged,
		opts.ShareVolumes, opts.ReadOnlyVolumes, append([]string{}, opts.Env...), append([]string{}, opts.Tools...), append([]string{}, caps...), identity})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
