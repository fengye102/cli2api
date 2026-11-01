package providers

import "context"

// LoginInput carries the operator's answers for a login round that needs more
// than "open this URL": a phone number to send an SMS code to, the code itself,
// or a captcha proof the provider has to forward upstream.
//
// It rides on the context rather than becoming a parameter because the login
// interface is deliberately provider-neutral (StartLogin(ctx, accountID)), and
// because a provider package may not import the console or control layer that
// knows where these values came from. Same reasoning as the session hint in
// session.go: shared vocabulary lives here, the transport is the context.
type LoginInput struct {
	// Method is the console's login_method value, for providers that offer
	// more than one browser flow.
	Method string
	// Phone is the domestic-account phone number, digits only or with a
	// country code; the provider normalizes it.
	Phone string
	// Captcha is an already-solved Aliyun verification proof. Empty means the
	// provider must do without it.
	Captcha string
}

type loginInputKey struct{}

// WithLoginInput attaches the operator's answers to the context.
func WithLoginInput(ctx context.Context, input LoginInput) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loginInputKey{}, input)
}

// LoginInputFromContext reads the answers. A context without them yields the
// zero value, so callers never have to nil-check.
func LoginInputFromContext(ctx context.Context) LoginInput {
	if ctx == nil {
		return LoginInput{}
	}
	if value, ok := ctx.Value(loginInputKey{}).(LoginInput); ok {
		return value
	}
	return LoginInput{}
}
