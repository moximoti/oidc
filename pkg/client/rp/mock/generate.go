package mock

//go:generate mockgen -package mock -destination ./verifier.mock.go github.com/moximoti/oidc/pkg/rp Verifier
