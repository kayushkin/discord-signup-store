package discordsignup

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The registry gives the command what its thirteen environment reads gave it
// before 2026-09-24: the same defaults with nothing set, and the operator's
// values when they are.
func TestTheRegistryReadsTheSameValuesTheCommandAlwaysDid(t *testing.T) {
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		SettingListenAddress:                      "127.0.0.1:8312",
		SettingDataDirectory:                      DefaultDataDir(),
		SettingApplicationPublicKey:               "",
		SettingApplicationID:                      "",
		SettingDiscordAPIBase:                     DiscordAPIBase,
		SettingAuthStoreURL:                       "http://127.0.0.1:8303",
		SettingAuthStoreToken:                     "",
		SettingBotTokenCredentialProvider:         "discord",
		SettingBotTokenCredentialAccount:          "default",
		SettingOAuthClientSecretCredentialAccount: "oauth-client",
		SettingOAuthRedirectURL:                   "",
		SettingDefaultTimezone:                    "",
	} {
		if got := unset.String(key); got != want {
			t.Errorf("%s with nothing set = %q, want %q", key, got, want)
		}
	}
	if unset.Boolean(SettingGatewayDisabled) {
		t.Error("the gateway is disabled with nothing set")
	}

	variables := map[string]string{
		"DISCORD_SIGNUP_ADDR":              "127.0.0.1:9999",
		"DISCORD_SIGNUP_DATA_DIR":          "/srv/discord",
		"DISCORD_APPLICATION_PUBLIC_KEY":   "abc123",
		"DISCORD_APPLICATION_ID":           "42",
		"DISCORD_API_BASE":                 "http://127.0.0.1:1/api",
		"AUTH_STORE_URL":                   "http://127.0.0.1:2",
		"AUTH_STORE_TOKEN":                 "a-token",
		"DISCORD_CREDENTIAL_PROVIDER":      "other-provider",
		"DISCORD_CREDENTIAL_ACCOUNT":       "other-bot",
		"DISCORD_OAUTH_CREDENTIAL_ACCOUNT": "other-client",
		"DISCORD_OAUTH_REDIRECT_URL":       "https://example.test/auth/callback",
		"DISCORD_DEFAULT_TIMEZONE":         "America/Los_Angeles",
		"DISCORD_GATEWAY_DISABLED":         "true",
	}
	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(variables))
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range SettingDefinitions() {
		if definition.Key == SettingGatewayDisabled {
			continue
		}
		if got := set.String(definition.Key); got != variables[definition.EnvironmentVariable] {
			t.Errorf("%s = %q, want %q from %s", definition.Key, got, variables[definition.EnvironmentVariable], definition.EnvironmentVariable)
		}
	}
	if !set.Boolean(SettingGatewayDisabled) {
		t.Error("DISCORD_GATEWAY_DISABLED=true left the gateway on")
	}

	// A variable set to the empty string is the same as unset, as it was when
	// the command compared os.Getenv to "".
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"DISCORD_SIGNUP_ADDR": "", "DISCORD_GATEWAY_DISABLED": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if empty.String(SettingListenAddress) != DefaultListenAddress || empty.Boolean(SettingGatewayDisabled) {
		t.Errorf("empty variables: listen=%q gateway disabled=%v", empty.String(SettingListenAddress), empty.Boolean(SettingGatewayDisabled))
	}
}

// The public key is the one setting the server cannot run without, and it has
// no default: the server's main stops on CheckRequired.
func TestThePublicKeyIsRequired(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "DISCORD_APPLICATION_PUBLIC_KEY") {
		t.Fatalf("CheckRequired with no public key = %v, want an error naming DISCORD_APPLICATION_PUBLIC_KEY", err)
	}
	registry, err = NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"DISCORD_APPLICATION_PUBLIC_KEY": "abc"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err != nil {
		t.Errorf("CheckRequired with the public key set = %v", err)
	}
}

// DISCORD_GATEWAY_DISABLED used to mean "any non-empty value", so "false"
// turned the gateway off. It is a boolean now: false keeps it on, and a word
// that is not a boolean stops the start rather than guessing.
func TestGatewayDisabledIsABoolean(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"DISCORD_GATEWAY_DISABLED": "false"}))
	if err != nil {
		t.Fatal(err)
	}
	if registry.Boolean(SettingGatewayDisabled) {
		t.Error("DISCORD_GATEWAY_DISABLED=false turned the gateway off")
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"DISCORD_GATEWAY_DISABLED": "yes"})); err == nil {
		t.Error("DISCORD_GATEWAY_DISABLED=yes was accepted")
	}
}

func TestTheRegistryRefusesADiscordVariableNobodyDeclared(t *testing.T) {
	// DISCORD_BOARD_CHANNEL_ID is one of the variables that moved into the
	// database on 2026-09-04; setting it again does nothing, so it is refused.
	for _, name := range []string{"DISCORD_SIGNUP_ADDRESS", "DISCORD_BOARD_CHANNEL_ID"} {
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{name: "x"}))
		if err == nil || !strings.Contains(err.Error(), name+" is set and discord-signup-store declares no such setting") {
			t.Errorf("NewSettingsRegistry with %s = %v, want a refusal naming it", name, err)
		}
	}
	// Names outside DISCORD_ are not this service's to refuse, including
	// auth-store's and si's.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"DISCORD_SIGNUP_ADDR": ":1", "AUTH_STORE_SOMETHING": "x", "SI_DISCORD_TOKEN": "x", "PATH": "/bin", "HOME": "/root",
	})); err != nil {
		t.Errorf("a declared variable and names outside the prefix were refused: %v", err)
	}
}

func TestGetSettingsDescribesTheServiceAndNothingCanBeWritten(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"DISCORD_SIGNUP_ADDR": "127.0.0.1:9999",
		"AUTH_STORE_TOKEN":    "the-auth-store-token-text",
	}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterSettingsHandler(mux, registry)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d: %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "the-auth-store-token-text") {
		t.Fatal("GET /api/settings served the auth-store token")
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(recorder.Body.Bytes(), &described); err != nil {
		t.Fatal(err)
	}
	if described.Service != ServiceName || len(described.Settings) != len(SettingDefinitions()) {
		t.Fatalf("service=%q with %d settings, want %q with %d", described.Service, len(described.Settings), ServiceName, len(SettingDefinitions()))
	}
	for _, setting := range described.Settings {
		if setting.Editable {
			t.Errorf("%s is editable, and this service has no operator gate to put a write behind", setting.Key)
		}
		if setting.Key == SettingListenAddress && (setting.Value != "127.0.0.1:9999" || setting.Source != msg.ServiceSettingSourceEnvironment) {
			t.Errorf("listen address served as %q from %q", setting.Value, setting.Source)
		}
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/settings/"+SettingListenAddress, strings.NewReader(`{"value":":1"}`)))
	if recorder.Code == http.StatusOK {
		t.Errorf("PUT /api/settings/%s = 200: a write route is mounted", SettingListenAddress)
	}
	if got := registry.String(SettingListenAddress); got != "127.0.0.1:9999" {
		t.Errorf("the refused write changed the listen address to %q", got)
	}
}

// The settings sit under /api/, which the public vhost refuses. The browser
// surface's vhost proxies / to this service, so a mount outside /api/ would be
// on the internet.
func TestTheSettingsRouteIsUnderTheAPIPrefixTheVhostRefuses(t *testing.T) {
	if !strings.HasPrefix(SettingsMountPath, "/api/") {
		t.Fatalf("settings are mounted at %s, outside /api/, which discord.kayushkin.com publishes", SettingsMountPath)
	}
	vhost, err := os.ReadFile("nginx-discord-vhost.conf.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vhost), "location /api/ { deny all; }") {
		t.Fatal("the vhost template no longer refuses /api/, so the settings route would be published")
	}
}

// Every environment variable the service's own code reads by name is declared.
// A read that is not declared is invisible on the settings page and escapes the
// startup check.
func TestEveryEnvironmentVariableTheServiceReadsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, definition := range SettingDefinitions() {
		declared[definition.EnvironmentVariable] = true
	}
	// Read by name and not settings of this service.
	notSettings := map[string]bool{}

	filesRead := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		filesRead++
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall || len(call.Args) == 0 {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			packageName, isIdentifier := selector.X.(*ast.Ident)
			if !isIdentifier || packageName.Name != "os" || (selector.Sel.Name != "Getenv" && selector.Sel.Name != "LookupEnv") {
				return true
			}
			literal, isLiteral := call.Args[0].(*ast.BasicLit)
			if !isLiteral {
				t.Errorf("%s reads an environment variable whose name is computed, which no declaration can be held to", path)
				return true
			}
			name, _ := strconv.Unquote(literal.Value)
			if !declared[name] && !notSettings[name] {
				t.Errorf("%s reads %s, which SettingDefinitions does not declare", path, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The walk starts at the package directory, which is the repository root. If
	// the package moves, the walk would read nothing and pass.
	if _, err := os.Stat(filepath.Join("cmd", "discord-signup-store", "main.go")); err != nil {
		t.Fatalf("the scan starts somewhere that is not the repository root: %v", err)
	}
	if filesRead < 3 {
		t.Fatalf("the scan read %d files; it is not looking at the service", filesRead)
	}
}
