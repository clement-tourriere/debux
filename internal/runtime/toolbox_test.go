package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clement-tourriere/debux/internal/store"
	"github.com/moby/moby/client"
)

func TestImageStorageFormatAndEffectiveUser(t *testing.T) {
	const imageID = "sha256:0123456789abcdef"
	for _, backend := range []string{"", "mise-v1", "mise-v99"} {
		t.Run(backend, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": imageID, "Config": map[string]any{
					"User": "65534", "Labels": map[string]string{"io.debux.toolbox": backend},
				}})
			}))
			defer srv.Close()
			cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.55"))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			got, err := debugImageVolumes(t.Context(), cli, DebugOpts{Image: "debug:candidate"})
			if backend == "mise-v99" {
				if err == nil {
					t.Fatal("unknown storage format silently accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := store.VolumesForImage(imageID)
			if backend == "mise-v1" {
				want = store.ToolVolumesForImage(imageID, toolStoreScope(DebugOpts{User: "65534"}))
			}
			if got != want {
				t.Fatalf("storage/user selection: got %+v want %+v", got, want)
			}
		})
	}
}

func TestToolStoreScopePreservesToolsButSeparatesPrivileges(t *testing.T) {
	base := DebugOpts{User: "0", Profile: ProfileGeneral}
	for name, opts := range map[string]DebugOpts{
		"default root":            {},
		"root name":               {User: "root"},
		"tools and command":       {User: "0", Tools: []string{"yq@4.53.6"}, Command: []string{"yq"}},
		"target-specific options": {User: "0", Image: "a:new-image", Env: []string{"KEY=value"}, Fresh: true, ShareVolumes: true},
	} {
		if toolStoreScope(opts) != toolStoreScope(base) {
			t.Errorf("%s unnecessarily resets the persistent store", name)
		}
	}
	for name, opts := range map[string]DebugOpts{
		"uid":          {User: "65534"},
		"profile":      {Profile: ProfileRestricted},
		"privileged":   {Privileged: true},
		"capabilities": {CapAdd: []string{"SYS_ADMIN"}},
	} {
		if toolStoreScope(opts) == toolStoreScope(base) {
			t.Errorf("%s shares a store across a security boundary", name)
		}
	}
}

func TestToolboxMountsCannotBeShadowed(t *testing.T) {
	for _, path := range []string{"/", "/var", "/var/lib", "/var/lib/debux", "/var/lib/debux/mise", "/etc/debux", "/usr/bin", "/tmp/state", "/nix/store", "/var/lib/", "/var/lib/../lib/debux", "/.miserc.toml", "/mise.toml", "/.env"} {
		if !isReservedDebugMountPath(path) {
			t.Errorf("unsafe shared mount accepted: %s", path)
		}
	}
	for _, path := range []string{"/data", "/var/log", "/target/usr", "/target/var/lib/debux", "/var/lib/database"} {
		if isReservedDebugMountPath(path) {
			t.Errorf("unrelated data mount rejected: %s", path)
		}
	}
}

func TestVersionedToolNamesAndOptionRejection(t *testing.T) {
	if err := ValidateTools([]string{"node@24", "python3", "aqua:mikefarah/yq@4.53.6"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"--postinstall", "-g", "node;touch /tmp/pwn", "$(id)", "node@24\nyq"} {
		if ValidateTools([]string{name}) == nil {
			t.Errorf("unsafe tool accepted: %q", name)
		}
	}
}
