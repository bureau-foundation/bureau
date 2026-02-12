# Copyright 2026 The Bureau Authors
# SPDX-License-Identifier: Apache-2.0

{
  description = "Bureau - AI agent orchestration platform";

  nixConfig = {
    extra-substituters = [ "https://cache.infra.bureau.foundation" ];
    extra-trusted-public-keys = [
      "cache.infra.bureau.foundation-1:3hpghLePqloLp0qMpkgPy/i0gKiL/Sxl2dY8EHZgOeY="
    ];
  };

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
      lib = {
        baseRunnerPackages = pkgs: [
          pkgs.bubblewrap
          pkgs.tmux
        ];

        # Tools for coding agents working on projects inside sandboxes.
        # Extends baseRunnerPackages with a functional shell environment,
        # language runtimes, package managers, and common developer utilities.
        developerRunnerPackages =
          pkgs:
          self.lib.baseRunnerPackages pkgs
          ++ [
            # Shell foundation.
            pkgs.bash
            pkgs.coreutils
            pkgs.findutils
            pkgs.gnugrep
            pkgs.gnused
            pkgs.gawk
            pkgs.diffutils
            pkgs.patch
            pkgs.less
            pkgs.file
            pkgs.tree
            pkgs.which
            pkgs.nano

            # Search.
            pkgs.ripgrep
            pkgs.fd

            # Data processing.
            pkgs.jq
            pkgs.yq-go

            # Version control.
            pkgs.git
            pkgs.gh

            # Network.
            pkgs.curl

            # Language runtimes and package managers. Agents install
            # additional tools on-demand into /var/bureau/cache/ via
            # these package managers.
            pkgs.nodejs
            pkgs.python3
            pkgs.uv
            pkgs.bun

            # Archives and compression.
            pkgs.gnutar
            pkgs.gzip
            pkgs.xz
            pkgs.zstd
            pkgs.unzip
            pkgs.zip

            # Crypto.
            pkgs.openssl
          ];

        # Full machine administration toolkit. Extends developerRunnerPackages
        # with remote access, system inspection, debugging, and infrastructure
        # management tools. The sysadmin agent uses this tier.
        #
        # The Nix CLI is NOT included here — it lives on the host at
        # /nix/var/nix/profiles/default/bin/nix. The sysadmin template
        # bind-mounts /nix/store (ro) and the nix profile so the sandbox
        # uses the host's exact version. Same pattern for the Docker socket.
        sysadminRunnerPackages =
          pkgs:
          self.lib.developerRunnerPackages pkgs
          ++ [
            # Remote access and network debugging.
            pkgs.openssh
            pkgs.rsync
            pkgs.wget
            pkgs.socat
            pkgs.dnsutils
            pkgs.nmap
            pkgs.iproute2

            # System inspection and debugging.
            pkgs.procps
            pkgs.htop
            pkgs.strace
            pkgs.lsof
            pkgs.util-linux

            # Infrastructure management.
            pkgs.attic-client
            pkgs.nixfmt
            pkgs.docker-client
            pkgs.docker-compose

            # Encryption.
            pkgs.age
            pkgs.gnupg

            # Additional compression.
            pkgs.bzip2
            pkgs.p7zip

            # Utilities.
            pkgs.gettext
          ];
      };
    }
    // flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Pin Go to the exact version required by go.mod. nixpkgs-unstable
        # may lag behind by a patch release; this overlay bumps it.
        # This creates a custom derivation not in cache.nixos.org, so
        # the first CI run compiles Go from source (~3 min). Subsequent
        # runs hit the Nix binary cache (magic-nix-cache in CI, or a
        # self-hosted Attic instance once deployed).
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
        vendorHash = "sha256-+a7k1XpaXNSdxx7nqt4ALzkUuVneLsPulhYpzucHpyE=";

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
          "bureau-pipeline-executor"
          "bureau-test-agent"
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
            # Production environments are defined in the environment repo
            # (bureau-foundation/environment), which composes on top of this
            # base. For Buildbarn runners, prefer:
            #   bureau environment build workstation --out-link deploy/buildbarn/runner-env
            runner-env = pkgs.buildEnv {
              name = "bureau-runner-env";
              paths = self.lib.baseRunnerPackages pkgs;
            };

            # Integration test environment: runner-env plus a shell and
            # coreutils. Sandbox commands (pipeline steps, workspace scripts)
            # run via "sh -c" and need basic tools. Production environments
            # get these from the environment repo; this derivation provides
            # them for integration tests without pulling in the full repo.
            integration-test-env = pkgs.buildEnv {
              name = "bureau-integration-test-env";
              paths = self.lib.baseRunnerPackages pkgs ++ [
                pkgs.bash
                pkgs.coreutils
              ];
            };

            # Environment for coding agents working on projects. Includes
            # shell tools, language runtimes (Node.js, Python), package
            # managers (npm, uv, bun), search tools (ripgrep, fd), and
            # common developer utilities.
            developer-runner-env = pkgs.buildEnv {
              name = "bureau-developer-runner-env";
              paths = self.lib.developerRunnerPackages pkgs;
            };

            # Environment for the sysadmin agent. Includes everything in
            # developer-runner-env plus machine administration tools:
            # remote access (ssh, rsync), system inspection (htop, strace,
            # lsof), network debugging (nmap, socat, dig), and
            # infrastructure management (attic, docker, age).
            sysadmin-runner-env = pkgs.buildEnv {
              name = "bureau-sysadmin-runner-env";
              paths = self.lib.sysadminRunnerPackages pkgs;
            };
          };

        devShells.default = pkgs.mkShell {
          name = "bureau-dev";
          packages = [
            # Build system — bazelisk reads .bazelversion to fetch the
            # correct Bazel release (9.0.0). Wrapped as "bazel" so that
            # .bazelrc, scripts, and `nix develop --command` all work
            # without relying on shell aliases (which don't apply in
            # non-interactive invocations).
            (pkgs.writeShellScriptBin "bazel" ''exec ${pkgs.bazelisk}/bin/bazelisk "$@"'')
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
            pkgs.lychee
            pkgs.golangci-lint

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

          # Nix strips TMPDIR from the host environment. Git 2.52+'s SSH
          # signing uses TMPDIR for its signing buffer and falls back to
          # "/" when unset, causing "Permission denied" on commit.
          env.TMPDIR = "/tmp";

          shellHook = ''
            # nix develop replaces PATH with the dev shell's packages,
            # but the nix-daemon.sh profile script uses a guard variable
            # (__ETC_PROFILE_NIX_SOURCED) to run only once per shell.
            # The guard survives into the subshell while the PATH it added
            # does not, so the base nix binary disappears. Re-add it.
            if ! command -v nix > /dev/null 2>&1; then
              export PATH="/nix/var/nix/profiles/default/bin:$PATH"
            fi

            # Install pre-commit hooks on first shell entry.
            if [ -f .pre-commit-config.yaml ] && [ -d .git ]; then
              pre-commit install --install-hooks > /dev/null 2>&1 || true
            fi
          '';
        };
      }
    );
}
