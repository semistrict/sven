// Package provider builds the System One client a config asks for, taking
// credentials from the environment.
package provider

import (
	"cmp"
	"fmt"
	"os"

	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/systemone"
)

// New returns a client for provider and model, an empty model meaning the
// provider's default. SVEN_PROVIDER and SVEN_MODEL override both.
func New(provider, model string) (*systemone.Client, error) {
	provider = cmp.Or(os.Getenv("SVEN_PROVIDER"), provider)
	var opts []systemone.Option
	if model := cmp.Or(os.Getenv("SVEN_MODEL"), model); model != "" {
		opts = append(opts, systemone.WithModel(model))
	}
	switch provider {
	case config.TypeSafe:
		key, err := env("TYPESAFE_API_KEY")
		if err != nil {
			return nil, err
		}
		if url := os.Getenv("TYPESAFE_BASE_URL"); url != "" {
			opts = append(opts, systemone.WithBaseURL(url))
		}
		return systemone.TypeSafe(key, opts...), nil
	case config.Cloudflare:
		account, err := env("CLOUDFLARE_ACCOUNT_ID")
		if err != nil {
			return nil, err
		}
		token, err := env("CLOUDFLARE_API_TOKEN")
		if err != nil {
			return nil, err
		}
		if url := os.Getenv("CLOUDFLARE_API_BASE_URL"); url != "" {
			opts = append(opts, systemone.WithBaseURL(url))
		}
		return systemone.Cloudflare(account, token, opts...), nil
	}
	return nil, fmt.Errorf("unknown provider %q: want %s or %s", provider, config.TypeSafe, config.Cloudflare)
}

func env(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	return v, nil
}
