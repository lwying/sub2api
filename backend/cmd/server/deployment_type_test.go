package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The deployment marker decides whether the admin API may replace the running
// binary, so the build-time value must never be weakened by the environment.
func TestResolveDeploymentType(t *testing.T) {
	original := DeploymentType
	t.Cleanup(func() { DeploymentType = original })

	t.Run("build-time marker wins over the environment", func(t *testing.T) {
		t.Setenv(deploymentTypeEnv, "native")
		DeploymentType = "docker"

		resolveDeploymentType()

		require.Equal(t, "docker", DeploymentType)
	})

	t.Run("image marker fills in when the build injected none", func(t *testing.T) {
		t.Setenv(deploymentTypeEnv, " docker ")
		DeploymentType = ""

		resolveDeploymentType()

		require.Equal(t, "docker", DeploymentType,
			"images that package a prebuilt binary declare the marker through the environment")
	})

	t.Run("a native build with no marker stays a native build", func(t *testing.T) {
		t.Setenv(deploymentTypeEnv, "")
		DeploymentType = ""

		resolveDeploymentType()

		require.Empty(t, DeploymentType)
	})
}
