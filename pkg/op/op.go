package op

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/schema"
	"golang.org/x/text/language"
	"gopkg.in/square/go-jose.v2"

	"github.com/caos/oidc/pkg/oidc"
	"github.com/caos/oidc/pkg/utils"
)

const (
	healthEndpoint               = "/healthz"
	readinessEndpoint            = "/ready"
	defaultAuthorizationEndpoint = "authorize"
	defaultTokenEndpoint         = "oauth/token"
	defaultIntrospectEndpoint    = "oauth/introspect"
	defaultUserinfoEndpoint      = "userinfo"
	defaultEndSessionEndpoint    = "end_session"
	defaultKeysEndpoint          = "keys"
)

var (
	DefaultEndpoints = &endpoints{
		Authorization: NewEndpoint(defaultAuthorizationEndpoint),
		Token:         NewEndpoint(defaultTokenEndpoint),
		Introspection: NewEndpoint(defaultIntrospectEndpoint),
		Userinfo:      NewEndpoint(defaultUserinfoEndpoint),
		EndSession:    NewEndpoint(defaultEndSessionEndpoint),
		JwksURI:       NewEndpoint(defaultKeysEndpoint),
	}
)

type OpenIDProvider interface {
	Configuration
	Storage() Storage
	Decoder() utils.Decoder
	Encoder() utils.Encoder
	IDTokenHintVerifier() IDTokenHintVerifier
	AccessTokenVerifier() AccessTokenVerifier
	Crypto() Crypto
	DefaultLogoutRedirectURI() string
	Signer() Signer
	Probes() []ProbesFn
	HttpHandler() http.Handler
}

type HttpInterceptor func(http.Handler) http.Handler

var allowAllOrigins = func(_ string) bool {
	return true
}

func CreateRouter(o OpenIDProvider, interceptors ...HttpInterceptor) *mux.Router {
	intercept := buildInterceptor(interceptors...)
	router := mux.NewRouter()
	router.Use(handlers.CORS(
		handlers.AllowCredentials(),
		handlers.AllowedHeaders([]string{"authorization", "content-type"}),
		handlers.AllowedOriginValidator(allowAllOrigins),
	))
	router.HandleFunc(healthEndpoint, healthHandler)
	router.HandleFunc(readinessEndpoint, readyHandler(o.Probes()))
	router.HandleFunc(oidc.DiscoveryEndpoint, discoveryHandler(o, o.Signer()))
	router.Handle(o.AuthorizationEndpoint().Relative(), intercept(authorizeHandler(o)))
	router.NewRoute().Path(o.AuthorizationEndpoint().Relative()+"/callback").Queries("id", "{id}").Handler(intercept(authorizeCallbackHandler(o)))
	router.Handle(o.TokenEndpoint().Relative(), intercept(tokenHandler(o)))
	router.HandleFunc(o.IntrospectionEndpoint().Relative(), introspectionHandler(o))
	router.HandleFunc(o.UserinfoEndpoint().Relative(), userinfoHandler(o))
	router.Handle(o.EndSessionEndpoint().Relative(), intercept(endSessionHandler(o)))
	router.HandleFunc(o.KeysEndpoint().Relative(), keysHandler(o.Storage()))
	return router
}

type Config struct {
	Issuer                   string
	CryptoKey                [32]byte
	DefaultLogoutRedirectURI string
	CodeMethodS256           bool
	AuthMethodPrivateKeyJWT  bool
	GrantTypeRefreshToken    bool
	SupportedUILocales       []language.Tag
}

type endpoints struct {
	Authorization      Endpoint
	Token              Endpoint
	Introspection      Endpoint
	Userinfo           Endpoint
	EndSession         Endpoint
	CheckSessionIframe Endpoint
	JwksURI            Endpoint
}

func NewOpenIDProvider(ctx context.Context, config *Config, storage Storage, opOpts ...Option) (OpenIDProvider, error) {
	err := ValidateIssuer(config.Issuer)
	if err != nil {
		return nil, err
	}

	o := &openidProvider{
		config:    config,
		storage:   storage,
		endpoints: DefaultEndpoints,
		timer:     make(<-chan time.Time),
	}

	for _, optFunc := range opOpts {
		if err := optFunc(o); err != nil {
			return nil, err
		}
	}

	keyCh := make(chan jose.SigningKey)
	o.signer = NewSigner(ctx, storage, keyCh)
	go storage.GetSigningKey(ctx, keyCh)

	o.httpHandler = CreateRouter(o, o.interceptors...)

	o.decoder = schema.NewDecoder()
	o.decoder.IgnoreUnknownKeys(true)

	o.encoder = schema.NewEncoder()

	o.crypto = NewAESCrypto(config.CryptoKey)

	return o, nil
}

type openidProvider struct {
	config              *Config
	endpoints           *endpoints
	storage             Storage
	signer              Signer
	idTokenHintVerifier IDTokenHintVerifier
	jwtProfileVerifier  JWTProfileVerifier
	accessTokenVerifier AccessTokenVerifier
	keySet              *openIDKeySet
	crypto              Crypto
	httpHandler         http.Handler
	decoder             *schema.Decoder
	encoder             *schema.Encoder
	interceptors        []HttpInterceptor
	retry               func(int) (bool, int)
	timer               <-chan time.Time
}

func (o *openidProvider) Issuer() string {
	return o.config.Issuer
}

func (o *openidProvider) AuthorizationEndpoint() Endpoint {
	return o.endpoints.Authorization
}

func (o *openidProvider) TokenEndpoint() Endpoint {
	return o.endpoints.Token
}

func (o *openidProvider) IntrospectionEndpoint() Endpoint {
	return o.endpoints.Introspection
}

func (o *openidProvider) UserinfoEndpoint() Endpoint {
	return o.endpoints.Userinfo
}

func (o *openidProvider) EndSessionEndpoint() Endpoint {
	return o.endpoints.EndSession
}

func (o *openidProvider) KeysEndpoint() Endpoint {
	return o.endpoints.JwksURI
}

func (o *openidProvider) AuthMethodPostSupported() bool {
	return true //todo: config
}

func (o *openidProvider) CodeMethodS256Supported() bool {
	return o.config.CodeMethodS256
}

func (o *openidProvider) AuthMethodPrivateKeyJWTSupported() bool {
	return o.config.AuthMethodPrivateKeyJWT
}

func (o *openidProvider) GrantTypeRefreshTokenSupported() bool {
	return o.config.GrantTypeRefreshToken
}

func (o *openidProvider) GrantTypeTokenExchangeSupported() bool {
	return false
}

func (o *openidProvider) GrantTypeJWTAuthorizationSupported() bool {
	return true
}

func (o *openidProvider) SupportedUILocales() []language.Tag {
	return o.config.SupportedUILocales
}

func (o *openidProvider) Storage() Storage {
	return o.storage
}

func (o *openidProvider) Decoder() utils.Decoder {
	return o.decoder
}

func (o *openidProvider) Encoder() utils.Encoder {
	return o.encoder
}

func (o *openidProvider) IDTokenHintVerifier() IDTokenHintVerifier {
	if o.idTokenHintVerifier == nil {
		o.idTokenHintVerifier = NewIDTokenHintVerifier(o.Issuer(), o.openIDKeySet())
	}
	return o.idTokenHintVerifier
}

func (o *openidProvider) JWTProfileVerifier() JWTProfileVerifier {
	if o.jwtProfileVerifier == nil {
		o.jwtProfileVerifier = NewJWTProfileVerifier(o.Storage(), o.Issuer(), 1*time.Hour, time.Second)
	}
	return o.jwtProfileVerifier
}

func (o *openidProvider) AccessTokenVerifier() AccessTokenVerifier {
	if o.accessTokenVerifier == nil {
		o.accessTokenVerifier = NewAccessTokenVerifier(o.Issuer(), o.openIDKeySet())
	}
	return o.accessTokenVerifier
}

func (o *openidProvider) openIDKeySet() oidc.KeySet {
	if o.keySet == nil {
		o.keySet = &openIDKeySet{o.Storage()}
	}
	return o.keySet
}

func (o *openidProvider) Crypto() Crypto {
	return o.crypto
}

func (o *openidProvider) DefaultLogoutRedirectURI() string {
	return o.config.DefaultLogoutRedirectURI
}

func (o *openidProvider) Signer() Signer {
	return o.signer
}

func (o *openidProvider) Probes() []ProbesFn {
	return []ProbesFn{
		ReadySigner(o.Signer()),
		ReadyStorage(o.Storage()),
	}
}

func (o *openidProvider) HttpHandler() http.Handler {
	return o.httpHandler
}

type openIDKeySet struct {
	Storage
}

//VerifySignature implements the oidc.KeySet interface
//providing an implementation for the keys stored in the OP Storage interface
func (o *openIDKeySet) VerifySignature(ctx context.Context, jws *jose.JSONWebSignature) ([]byte, error) {
	keySet, err := o.Storage.GetKeySet(ctx)
	if err != nil {
		return nil, errors.New("error fetching keys")
	}
	keyID, alg := oidc.GetKeyIDAndAlg(jws)
	key, ok := oidc.FindKey(keyID, oidc.KeyUseSignature, alg, keySet.Keys...)
	if !ok {
		return nil, errors.New("invalid kid")
	}
	return jws.Verify(&key)
}

type Option func(o *openidProvider) error

func WithCustomAuthEndpoint(endpoint Endpoint) Option {
	return func(o *openidProvider) error {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		o.endpoints.Authorization = endpoint
		return nil
	}
}

func WithCustomTokenEndpoint(endpoint Endpoint) Option {
	return func(o *openidProvider) error {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		o.endpoints.Token = endpoint
		return nil
	}
}

func WithCustomIntrospectionEndpoint(endpoint Endpoint) Option {
	return func(o *openidProvider) error {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		o.endpoints.Introspection = endpoint
		return nil
	}
}

func WithCustomUserinfoEndpoint(endpoint Endpoint) Option {
	return func(o *openidProvider) error {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		o.endpoints.Userinfo = endpoint
		return nil
	}
}

func WithCustomEndSessionEndpoint(endpoint Endpoint) Option {
	return func(o *openidProvider) error {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		o.endpoints.EndSession = endpoint
		return nil
	}
}

func WithCustomKeysEndpoint(endpoint Endpoint) Option {
	return func(o *openidProvider) error {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		o.endpoints.JwksURI = endpoint
		return nil
	}
}

func WithCustomEndpoints(auth, token, userInfo, endSession, keys Endpoint) Option {
	return func(o *openidProvider) error {
		o.endpoints.Authorization = auth
		o.endpoints.Token = token
		o.endpoints.Userinfo = userInfo
		o.endpoints.EndSession = endSession
		o.endpoints.JwksURI = keys
		return nil
	}
}

func WithHttpInterceptors(interceptors ...HttpInterceptor) Option {
	return func(o *openidProvider) error {
		o.interceptors = append(o.interceptors, interceptors...)
		return nil
	}
}

func buildInterceptor(interceptors ...HttpInterceptor) func(http.HandlerFunc) http.Handler {
	return func(handlerFunc http.HandlerFunc) http.Handler {
		handler := handlerFuncToHandler(handlerFunc)
		for i := len(interceptors) - 1; i >= 0; i-- {
			handler = interceptors[i](handler)
		}
		return handler
	}
}

func handlerFuncToHandler(handlerFunc http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerFunc(w, r)
	})
}
