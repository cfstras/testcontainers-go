package testcontainers_test

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/image"
)

func TestSubstituteBuiltImage(t *testing.T) {
	req := testcontainers.Request{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    "testdata",
			Dockerfile: "echo.Dockerfile",
			Tag:        "my-image",
			Repo:       "my-repo",
		},
		ImageSubstitutors: []image.Substitutor{image.NewPrependHubRegistry("my-registry")},
		Started:           false,
	}

	t.Run("should not use the properties prefix on built images", func(t *testing.T) {
		c, err := testcontainers.New(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}

		json, err := c.Inspect(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		if json.Config.Image != "my-registry/my-repo:my-image" {
			t.Errorf("expected my-registry/my-repo:my-image, got %s", json.Config.Image)
		}
	})
}