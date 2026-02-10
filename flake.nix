# Copyright 2026 The Bureau Authors
# SPDX-License-Identifier: Apache-2.0

{
  description = "Bureau - AI agent orchestration platform";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    # Base packages required by Bureau's own tests in any execution
    # environment (Buildbarn runner, sandbox, etc.). The environment repo
    # (bureau-foundation/environment) composes on top of this list —
    # project-specific tools go there, not here.
    #
    # This is a function, not a derivation, so it lives outside the
    # per-system wrapper. Callers pass their own pkgs to get the right
    # system's packages.
    {
      lib.baseRunnerPackages = pkgs: [ pkgs.tmux ];
    }
    // flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Pin Go to the exact version required by go.mod. nixpkgs-unstable
        # may lag behind by a patch release; this overlay bumps it.
        go = pkgs.go_1_25.overrideAttrs (_: rec {
          version = "1.25.6";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go${version}.src.tar.gz";
            hash = "sha256-WMv3ceRNdt5vVtGeM7d9dFoeSJNAkih15GWFuXXCsFk=";
          };
        });

        version = self.shortRev or self.dirtyShortRev or "dev";

        # All Bureau binaries share the same Go module. Each gets its own
        # derivation so the daemon can track per-binary store paths, but they
        # share the same vendor hash (same go.sum).
        #
        # Uses buildGoModule rather than invoking Bazel: Bazel needs network
        # access for toolchain downloads which is forbidden in Nix's build
        # sandbox. The resulting binaries are identical (same Go compiler,
        # CGO_ENABLED=0, static linking). The daemon compares binary content
        # hashes to decide what needs restarting, not store paths or build
        # provenance.
        vendorHash = "sha256-U+Y75CvMASy2AL7vuMRXr/1HBMiKsKFucReAccULJsw=";

        # Override the Go version used by buildGoModule. The `go` parameter
        # is a callPackage injection — passing it as a build attribute does
        # nothing. `.override` replaces the callPackage argument so both the
        # vendor-fetch derivation and the main build use our pinned Go.
        buildGoModule = pkgs.buildGoModule.override { inherit go; };

        mkBureauBinary =
          name:
          buildGoModule {
            pname = name;
            inherit version;
            src = ./.;
            inherit vendorHash;
            subPackages = [ "cmd/${name}" ];
            env.CGO_ENABLED = 0;
            ldflags = [
              "-s"
              "-w"
            ];

            # Tests run through Bazel (which provides test binaries via
            # data deps and tmux via test environment). Nix builds are
            # for producing release binaries only.
            doCheck = false;
          };

        binaries = [
          "bureau"
          "bureau-daemon"
          "bureau-launcher"
          "bureau-proxy"
          "bureau-bridge"
          "bureau-sandbox"
          "bureau-credentials"
          "bureau-proxy-call"
          "bureau-observe-relay"
        ];
      in
      {
        packages =
          builtins.listToAttrs (
            map (name: {
              inherit name;
              value = mkBureauBinary name;
            }) binaries
          )
          // {
            default = self.packages.${system}.bureau;

            # Minimal environment for Bureau's own CI and local development.
            # Contains only what Bureau's tests need (see lib.baseRunnerPackages).
            # Production and multi-project environments are defined in the
            # bureau-foundation/environment repo, which composes on top of
            # this base. Build with:
            #   nix build .#runner-env --out-link deploy/buildbarn/runner-env
            runner-env = pkgs.buildEnv {
              name = "bureau-runner-env";
              paths = self.lib.baseRunnerPackages pkgs;
            };
          };

        devShells.default = pkgs.mkShell {
          name = "bureau-dev";
          packages = [
            # Build system — bazelisk reads .bazelversion to fetch the
            # correct Bazel release (9.0.0). No need to pin Bazel in Nix.
            pkgs.bazelisk
            pkgs.buildifier

            # Go tooling for IDE support and ad-hoc testing. Uses our
            # pinned Go version to match go.mod.
            go
            pkgs.gopls
            pkgs.gotools
            pkgs.delve

            # Testing dependencies.
            pkgs.tmux

            # Infrastructure management.
            pkgs.docker-compose

            # Code quality.
            pkgs.pre-commit
            pkgs.shellcheck

            # Version control.
            pkgs.git
            pkgs.gh

            # Utilities.
            pkgs.jq
            pkgs.curl
            pkgs.openssl

            # Nix formatting.
            pkgs.nixfmt

            # Attic CLI for pushing to the binary cache.
            pkgs.attic-client
          ];

          env.CGO_ENABLED = "0";

          shellHook = ''
            # Install pre-commit hooks on first shell entry.
            if [ -f .pre-commit-config.yaml ] && [ -d .git ]; then
              pre-commit install --install-hooks > /dev/null 2>&1 || true
            fi

            # bazelisk needs to be invoked as "bazel" for .bazelrc and
            # tooling compatibility.
            alias bazel=bazelisk
          '';
        };
      }
    );
}
