package discordsignup

import (
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// ServiceName is this service's name in its own settings description, as
// healthcheck and the repo know it.
const ServiceName = "discord-signup-store"

// OwnedEnvironmentVariablePrefix is the prefix of the variables that are this
// service's alone. A set variable carrying it that SettingDefinitions does not
// declare stops the service from starting: it is a misspelling or a leftover,
// such as the board and reminder channel variables that moved into the
// database on 2026-09-04, and either way someone believes it does something.
//
// It is the whole of DISCORD_ because no other service here reads a bare
// DISCORD_ name (si's are SI_DISCORD_), and this process's environment comes
// only from its own unit and its own environment file. AUTH_STORE_URL and
// AUTH_STORE_TOKEN are declared and not owned: every caller of auth-store reads
// the same names.
const OwnedEnvironmentVariablePrefix = "DISCORD_"

// Keys of the settings, as GET /api/settings names them.
const (
	SettingListenAddress                      = "listen_address"
	SettingDataDirectory                      = "data_directory"
	SettingApplicationPublicKey               = "application_public_key"
	SettingApplicationID                      = "application_id"
	SettingDiscordAPIBase                     = "discord_api_base"
	SettingAuthStoreURL                       = "auth_store_url"
	SettingAuthStoreToken                     = "auth_store_token"
	SettingBotTokenCredentialProvider         = "bot_token_credential_provider"
	SettingBotTokenCredentialAccount          = "bot_token_credential_account"
	SettingOAuthClientSecretCredentialAccount = "oauth_client_secret_credential_account"
	SettingOAuthRedirectURL                   = "oauth_redirect_url"
	SettingDefaultTimezone                    = "default_timezone"
	SettingGatewayDisabled                    = "gateway_disabled"
)

// DefaultListenAddress is where the service listens with nothing set. Loopback,
// deliberately: the admin routes edit rosters and have no auth of their own.
const DefaultListenAddress = "127.0.0.1:8312"

// DefaultAuthStoreURL is where auth-store is asked for the bot token and the
// OAuth client secret with nothing set.
const DefaultAuthStoreURL = "http://127.0.0.1:8303"

// SettingsMountPath is where the service describes its settings. It is under
// /api/ and not at /settings, the path most services use: the public vhost for
// the browser surface proxies / to this service and refuses only /api/, so a
// bare /settings would describe this host's wiring to the internet.
const SettingsMountPath = "/api/settings"

// SettingDefinitions declares every environment variable this process reads,
// once. The command reads its configuration from it; GET /api/settings
// describes the service from it; and a test holds every os.Getenv in the repo
// to it, so a variable cannot be read without being declared here.
//
// Nothing here is Editable, and nothing may become so while the admin routes
// have no operator gate: GET /api/settings is as open as every other /api/
// route, which is to say open to anything on this host and to nothing outside.
func SettingDefinitions() []servicesettings.Definition {
	return []servicesettings.Definition{
		{Key: SettingListenAddress, EnvironmentVariable: "DISCORD_SIGNUP_ADDR", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultListenAddress,
			Description: "The address the HTTP server listens on. nginx proxies /discord/interactions and the browser surface to it, so changing it means changing both vhosts. A wildcard address would put the unauthenticated roster API on the network."},
		{Key: SettingDataDirectory, EnvironmentVariable: "DISCORD_SIGNUP_DATA_DIR", Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultDataDir(),
			Description: "The directory that holds discord-signup-store.db. Changing it starts the service on whatever database is there, or an empty one; the old events and rosters stay where they were."},
		{Key: SettingApplicationPublicKey, EnvironmentVariable: "DISCORD_APPLICATION_PUBLIC_KEY", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Required: true,
			Description: "The Discord application's Ed25519 public key, from the Developer Portal under General Information. Every interaction is verified against it; a wrong one makes every request from Discord fail with 401. Public by design, so it is not a secret."},
		{Key: SettingApplicationID, EnvironmentVariable: "DISCORD_APPLICATION_ID", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The Discord application's id, used as the OAuth client id for the browser login. Read only when the OAuth redirect URL is set."},
		{Key: SettingDiscordAPIBase, EnvironmentVariable: "DISCORD_API_BASE", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DiscordAPIBase,
			Description: "The versioned root of Discord's REST API. Changing it points every push to Discord somewhere else, which is only useful for a test double."},
		{Key: SettingAuthStoreURL, EnvironmentVariable: "AUTH_STORE_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: DefaultAuthStoreURL,
			Description: "Where auth-store is asked for the bot token and the OAuth client secret. Asked on first use and again after a 401, not at start."},
		{Key: SettingAuthStoreToken, EnvironmentVariable: "AUTH_STORE_TOKEN", Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The bearer token sent to auth-store. Unset, auth-store refuses the lookups, and every push to Discord and every browser login fails; buttons, rosters and the API keep working."},
		{Key: SettingBotTokenCredentialProvider, EnvironmentVariable: "DISCORD_CREDENTIAL_PROVIDER", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "discord",
			Description: "The auth-store provider both Discord credentials are filed under."},
		{Key: SettingBotTokenCredentialAccount, EnvironmentVariable: "DISCORD_CREDENTIAL_ACCOUNT", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "default",
			Description: "The auth-store account that holds the bot token. Changing it makes the service act in Discord as whichever bot that row belongs to."},
		{Key: SettingOAuthClientSecretCredentialAccount, EnvironmentVariable: "DISCORD_OAUTH_CREDENTIAL_ACCOUNT", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString, Default: "oauth-client",
			Description: "The auth-store account that holds the OAuth client secret for the browser login. A different row from the bot token on purpose: one mints user logins, the other acts in every server."},
		{Key: SettingOAuthRedirectURL, EnvironmentVariable: "DISCORD_OAUTH_REDIRECT_URL", Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The browser login's callback URL, as registered in the Developer Portal. Unset turns the browser surface off: the login routes answer 501 and everything else keeps working."},
		{Key: SettingDefaultTimezone, EnvironmentVariable: "DISCORD_DEFAULT_TIMEZONE", Kind: msg.ServiceSettingKindBehaviour, ValueType: msg.ServiceSettingValueTypeString,
			Description: "The IANA zone a time typed into a Discord form is read in, such as America/Los_Angeles. Unset reads times as UTC and says so at start. A name that is not a zone stops the start."},
		{Key: SettingGatewayDisabled, EnvironmentVariable: "DISCORD_GATEWAY_DISABLED", Kind: msg.ServiceSettingKindBehaviour, ValueType: msg.ServiceSettingValueTypeBoolean, Default: "false",
			Description: "true stops the service from opening Discord's gateway socket. Without it, Discord's own Interested button no longer feeds the roster; buttons, rosters and the API keep working."},
	}
}

// NewSettingsRegistry reads this service's settings from environment. It fails
// on a value that does not parse and on a set DISCORD_ variable nobody
// declared. It does not check the required public key; the server's main does,
// with CheckRequired.
func NewSettingsRegistry(environment servicesettings.Environment) (*servicesettings.Registry, error) {
	return servicesettings.New(ServiceName, []string{OwnedEnvironmentVariablePrefix}, SettingDefinitions(), environment)
}

// RegisterSettingsHandler serves the registry at GET /api/settings. PUT is not
// mounted: no setting is Editable, so there is nothing a write could change.
func RegisterSettingsHandler(mux *http.ServeMux, registry *servicesettings.Registry) {
	mux.Handle("GET "+SettingsMountPath, servicesettings.Handler(registry, SettingsMountPath))
}
