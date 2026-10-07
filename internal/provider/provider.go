// Package provider builds the System One client a config asks for, taking
// credentials from the environment.
package provider

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/semistrict/sven/internal/config"
	"github.com/semistrict/sven/systemone"
)

// New returns a client for provider and model, an empty model meaning the
// provider's default. The free sven API needs the project's consent to
// storing requests and responses.
func New(provider, model string, allowRequestStorage bool) (*systemone.Client, error) {
	var opts []systemone.Option
	if model != "" {
		opts = append(opts, systemone.WithModel(model))
	}
	switch provider {
	case config.Sven:
		if !allowRequestStorage {
			return nil, errors.New("the free sven API stores the requests and responses it handles: run `sven init` to agree, or use your own key with provider: typesafe and TYPESAFE_API_KEY")
		}
		if url := os.Getenv("SVEN_BASE_URL"); url != "" {
			opts = append(opts, systemone.WithBaseURL(url))
		}
		return systemone.Sven(opts...), nil
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
	case config.OpenAI:
		key, err := env("OPENAI_API_KEY")
		if err != nil {
			return nil, err
		}
		if url := os.Getenv("OPENAI_BASE_URL"); url != "" {
			opts = append(opts, systemone.WithBaseURL(url))
		}
		return systemone.OpenAI(key, opts...), nil
	}
	return nil, fmt.Errorf("unknown provider %q: want %s", provider, strings.Join(config.Providers, ", "))
}

func env(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	return v, nil
}
