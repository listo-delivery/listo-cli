package login

import (
	"context"
	"errors"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/config"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

// Margin is how much life an access token must have left to be presented as is.
const Margin = 60 * time.Second

// GrantSource is the api.TokenSource of the grant stored for one mode.
//
// THE ONE RULE: a refresh token is single use, and the server ends the whole
// grant when one it has already rotated away comes back. So every path that
// presents a refresh token does it inside Store.WithLock and reads the record
// AGAIN after the lock is taken: a token another terminal rotated while this
// one waited is simply used, never presented.
type GrantSource struct {
	Store *config.Store
	Mode  config.Mode
	Flow  *Flow
	Now   func() time.Time // default time.Now
}

var _ api.TokenSource = (*GrantSource)(nil)

func (s *GrantSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *GrantSource) fresh(c *config.Credential) bool {
	return c != nil && c.AccessToken != "" && s.now().Add(Margin).Before(c.AccessTokenExpiresAt)
}

// Token answers the stored access token while it has more than a minute left,
// and refreshes otherwise.
func (s *GrantSource) Token(ctx context.Context) (string, error) {
	rec, err := s.Store.Load(s.Mode)
	if err != nil {
		return "", loadFailure(err)
	}
	if s.fresh(rec.Credential) {
		return rec.Credential.AccessToken, nil
	}
	return s.refresh(ctx, "")
}

// Rejected is told the API answered 401 to the bearer it sent. It refreshes once
// and asks for a retry, unless another terminal already replaced that bearer, in
// which case the new one is used without a second refresh.
func (s *GrantSource) Rejected(ctx context.Context, token string) (bool, error) {
	_, err := s.refresh(ctx, token)
	if err != nil {
		return false, err
	}
	return true, nil
}

// refresh returns a usable access token. rejected is the bearer the API just
// refused ("" when the stored one is merely close to its end).
func (s *GrantSource) refresh(ctx context.Context, rejected string) (string, error) {
	var access string
	err := s.Store.WithLock(ctx, func(tx *config.Tx) error {
		rec, err := tx.Load(s.Mode) // AFTER the lock
		if err != nil {
			return loadFailure(err)
		}
		c := rec.Credential
		if !c.IsGrant() || c.RefreshToken == "" {
			return sessionEnded(s.Mode)
		}
		// Another terminal got here first: its token is newer than the one that
		// was refused (or than the one this call found stale).
		if s.fresh(c) && c.AccessToken != rejected {
			access = c.AccessToken
			return nil
		}
		tok, err := s.Flow.Refresh(ctx, c.RefreshToken)
		if err != nil {
			var oe *OAuthError
			if errors.As(err, &oe) && oe.Code == "invalid_grant" {
				// The grant is over (revoked, expired or reused): the stored copy is
				// useless, and keeping it would send every later command here again.
				// The webhook secret stays: it belongs to the computer, not the grant.
				rec.Credential = nil
				if rec.WebhookSecret == "" {
					_ = tx.Delete(s.Mode)
				} else {
					_, _ = tx.Save(s.Mode, rec)
				}
				return sessionEnded(s.Mode)
			}
			var te *api.TransportError
			if errors.As(err, &te) {
				return err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return output.Fail("login.refresh_failed", i18n.Args{"detail": err.Error()}).Next("error.next.retry").Wrap(err)
		}
		next := *c
		next.AccessToken = tok.AccessToken
		next.AccessTokenExpiresAt = tok.ExpiresAt
		if tok.RefreshToken != "" {
			next.RefreshToken = tok.RefreshToken
		}
		rec.Credential = &next
		if _, err := tx.Save(s.Mode, rec); err != nil {
			// The refresh token was spent on the server and cannot be stored: this
			// login can no longer be continued. Say so rather than a file error.
			return output.Fail("login.save_failed", i18n.Args{"detail": err.Error()}).Next(nextLogin(s.Mode)).Wrap(err)
		}
		access = next.AccessToken
		return nil
	})
	if err != nil {
		return "", err
	}
	return access, nil
}

func sessionEnded(mode config.Mode) error {
	return output.Fail("login.session_ended").WithCode("session_ended").Next(nextLogin(mode))
}

func nextLogin(mode config.Mode) string {
	if mode == config.Live {
		return "auth.next_login_live"
	}
	return "auth.next_login"
}

func loadFailure(err error) error {
	return output.Fail("config.unreadable", i18n.Args{"detail": err.Error()}).Wrap(err)
}
