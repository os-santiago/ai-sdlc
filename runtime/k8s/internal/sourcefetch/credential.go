package sourcefetch

import "context"

type readCredentialKey struct{}
type readCredential struct{ repository, token string }

// WithReadCredential binds an explicitly supplied API read credential to one
// repository and call context. It never reads ambient environment or config.
// The caller must keep this context within the trusted source reader; neither
// the credential nor this context belongs in generated workloads or evidence.
func WithReadCredential(ctx context.Context, repository, token string) (context.Context, error) {
	if ctx == nil || token == "" {
		return nil, ErrSource
	}
	if _, err := New(repository, token); err != nil {
		return nil, ErrSource
	}
	if _, exists := ctx.Value(readCredentialKey{}).(readCredential); exists {
		return nil, ErrSource
	}
	return context.WithValue(ctx, readCredentialKey{}, readCredential{repository, token}), nil
}

func (c *Client) scoped(ctx context.Context) (*Client, error) {
	credential, exists := ctx.Value(readCredentialKey{}).(readCredential)
	if !exists {
		return c, nil
	}
	if credential.repository != c.repository || c.token != "" && c.token != credential.token {
		return nil, ErrSource
	}
	copy := *c
	copy.token = credential.token
	return &copy, nil
}
