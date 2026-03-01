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
    # Environment modules and presets. These are functions, not
    # derivations, so they live outside the per-system wrapper.
    # Callers pass their own pkgs to get the right system's packages.
    #
    # Modules are atomic building blocks (pkgs -> [derivation]).
    # Presets compose modules into useful groups.
    # The environment repo (bureau-foundation/environment) composes
    # on top of these — project-specific tools go there, not here.
    {
      lib = {
        # Apply all module functions in an attribute set to pkgs and
        # flatten into a single package list.
        applyModules =
          moduleSet: pkgs:
          builtins.concatLists (builtins.attrValues (builtins.mapAttrs (_: m: m pkgs) moduleSet));

        # Atomic modules: each is pkgs -> [derivation].
        # modules.<category>.<name> — always an atom, never a composition.
        modules = {
          # Foundation: tools any shell user expects.
          foundation = {
            shell = pkgs: [
              pkgs.bash
              pkgs.coreutils
              pkgs.findutils
              pkgs.gnugrep
              pkgs.gnused
              pkgs.gawk
              pkgs.diffutils
              pkgs.patch
            ];
            search = pkgs: [
              pkgs.ripgrep
              pkgs.fd
            ];
            data = pkgs: [
              pkgs.jq
              pkgs.yq-go
            ];
            archive = pkgs: [
              pkgs.gnutar
              pkgs.gzip
              pkgs.xz
              pkgs.zstd
              pkgs.bzip2
              pkgs.unzip
              pkgs.zip
            ];
            crypto = pkgs: [
              pkgs.openssl
            ];
            network = pkgs: [
              pkgs.curl
            ];
          };

          # Developer: tools for anyone writing code.
          developer = {
            vcs = pkgs: [
              pkgs.git
              pkgs.gh
            ];
            editor = pkgs: [
              pkgs.nano
            ];
            inspect = pkgs: [
              pkgs.less
              pkgs.file
              pkgs.tree
              pkgs.which
            ];
          };

          # Language runtimes: pick per-project.
          runtime = {
            nodejs = pkgs: [ pkgs.nodejs ];
            python = pkgs: [ pkgs.python3 ];
            uv = pkgs: [ pkgs.uv ];
            bun = pkgs: [ pkgs.bun ];
          };

          # System administration: for sysadmin agents.
          sysadmin = {
            remote = pkgs: [
              pkgs.openssh
              pkgs.rsync
              pkgs.wget
            ];
            network-debug = pkgs: [
              pkgs.nmap
              pkgs.socat
              pkgs.dnsutils
              pkgs.iproute2
            ];
            containers = pkgs: [
              pkgs.docker-client
              pkgs.docker-compose
            ];
            nix-tools = pkgs: [
              pkgs.attic-client
              pkgs.nixfmt
            ];
            system-inspect = pkgs: [
              pkgs.procps
              pkgs.htop
              pkgs.strace
              pkgs.lsof
              pkgs.util-linux
            ];
            crypto = pkgs: [
              pkgs.age
              pkgs.gnupg
            ];
            utils = pkgs: [
              pkgs.p7zip
              pkgs.gettext
            ];
          };
        };

        # Composed presets: each is pkgs -> [derivation].
        # presets.<name> — always a composition, never an atom.
        presets = {
          # Bare working shell. Minimum viable interactive environment.
          foundation-minimal = pkgs: self.lib.modules.foundation.shell pkgs;

          # Full foundation. Shell + search + data processing + archives +
          # crypto + network. What anyone interacting with a Linux system expects.
          foundation = self.lib.applyModules self.lib.modules.foundation;

          # Developer tools. Foundation + version control + editor + inspection.
          # Base for any coding agent or human developer.
          developer =
            pkgs: self.lib.presets.foundation pkgs ++ self.lib.applyModules self.lib.modules.developer pkgs;

          # Sysadmin tools. Developer + remote access + network debugging +
          # containers + Nix tooling + system inspection + encryption.
          # The Nix CLI itself is NOT included — it lives on the host at
          # /nix/var/nix/profiles/default/bin/nix. The sysadmin template
          # bind-mounts /nix/store (ro) and the nix profile so the sandbox
          # uses the host's exact version. Same pattern for the Docker socket.
          sysadmin =
            pkgs: self.lib.presets.developer pkgs ++ self.lib.applyModules self.lib.modules.sysadmin pkgs;
        };

        # Bureau-specific sandbox runtime dependencies. Not a general module —
        # these are Bureau's own binaries needed to manage sandboxes (bubblewrap
        # for namespace creation, tmux for observation relay).
        bureauRuntime = pkgs: [
          pkgs.bubblewrap
          pkgs.tmux
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
        vendorHash = "sha256-+TqNFI8OkgXTXCK8Wb0Bfy37YdJzM7fXfJNJGQ1FIp4=";

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
          "bureau-log-relay"
          "bureau-bridge"
          "bureau-sandbox"
          "bureau-credentials"
          "bureau-proxy-call"
          "bureau-observe-relay"
          "bureau-pipeline-executor"
          "bureau-agent-service"
          "bureau-artifact-service"
          "bureau-ticket-service"
          "bureau-test-agent"
        ];

        # Bureau binaries that run on the host outside sandboxes: the
        # daemon, launcher, and everything the launcher spawns (proxy,
        # sandbox creator, observation relay, bridge) plus the CLI and
        # managed services. Includes bubblewrap and tmux which the
        # launcher needs for namespace creation and terminal observation.
        hostBinaries = [
          "bureau"
          "bureau-daemon"
          "bureau-launcher"
          "bureau-proxy"
          "bureau-log-relay"
          "bureau-bridge"
          "bureau-sandbox"
          "bureau-observe-relay"
          "bureau-credentials"
          "bureau-agent-service"
          "bureau-artifact-service"
          "bureau-ticket-service"
        ];

        # Bureau binaries for use inside sandboxes by agents and
        # pipelines. The CLI lets agents interact with Bureau, proxy-call
        # makes HTTP requests through the per-sandbox credential proxy,
        # and the pipeline executor runs structured step sequences.
        sandboxBinaries = [
          "bureau"
          "bureau-bridge"
          "bureau-proxy-call"
          "bureau-pipeline-executor"
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

            # All Bureau host binaries in a single environment. The launcher
            # finds bureau-proxy, bureau-sandbox, and bureau-observe-relay
            # next to its own binary (same bin/ directory in the store
            # path), so PATH-based resolution is unnecessary. Use this for
            # deploying Bureau on a machine or running locally.
            bureau-host-env = pkgs.buildEnv {
              name = "bureau-host-env";
              paths = map (name: self.packages.${system}.${name}) hostBinaries ++ self.lib.bureauRuntime pkgs;
            };

            # Bureau's own tools for agents running inside sandboxes. The
            # runner-env packages below provide general-purpose tools
            # (shell, git, language runtimes); this provides the Bureau-
            # specific binaries that agents use to interact with the
            # platform.
            bureau-sandbox-env = pkgs.buildEnv {
              name = "bureau-sandbox-env";
              paths = map (name: self.packages.${system}.${name}) sandboxBinaries;
            };

            # Minimal environment for Bureau's own CI and Buildbarn runners.
            # Contains only Bureau's sandbox runtime deps (bubblewrap, tmux).
            # For Buildbarn runners, prefer:
            #   bureau environment build workstation --out-link deploy/buildbarn/runner-env
            runner-env = pkgs.buildEnv {
              name = "bureau-runner-env";
              paths = self.lib.bureauRuntime pkgs;
            };

            # Integration test environment: Bureau runtime + a working shell.
            # Sandbox commands (pipeline steps, workspace scripts) run via
            # "sh -c" and need basic tools. Production environments get a full
            # foundation from the environment repo; this provides just enough
            # for integration tests.
            integration-test-env = pkgs.buildEnv {
              name = "bureau-integration-test-env";
              paths =
                self.lib.bureauRuntime pkgs
                ++ self.lib.presets.foundation-minimal pkgs
                ++ self.lib.modules.developer.vcs pkgs;
            };

            # Environment for coding agents working on projects. Bureau
            # runtime + full developer preset + language runtimes (Node.js,
            # Python, uv, bun). Agents install additional tools on-demand
            # into /var/bureau/cache/ via these package managers.
            developer-runner-env = pkgs.buildEnv {
              name = "bureau-developer-runner-env";
              paths =
                self.lib.bureauRuntime pkgs
                ++ self.lib.presets.developer pkgs
                ++ self.lib.applyModules self.lib.modules.runtime pkgs;
            };

            # Environment for the sysadmin agent. Bureau runtime + sysadmin
            # preset (developer tools + remote access, network debugging,
            # containers, Nix tooling, system inspection, encryption) +
            # language runtimes.
            sysadmin-runner-env = pkgs.buildEnv {
              name = "bureau-sysadmin-runner-env";
              paths =
                self.lib.bureauRuntime pkgs
                ++ self.lib.presets.sysadmin pkgs
                ++ self.lib.applyModules self.lib.modules.runtime pkgs;
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
            pkgs.lefthook
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

            # Nix tooling.
            pkgs.nixfmt
            pkgs.statix
            pkgs.deadnix

            # Attic CLI for pushing to the binary cache.
            pkgs.attic-client
          ];

          env.CGO_ENABLED = "0";

          shellHook = ''
            # Nix stdenv's setup.sh sets TMPDIR="$NIX_BUILD_TOP". In a
            # dev shell NIX_BUILD_TOP is empty, so TMPDIR becomes "".
            # Git's SSH signing constructs "/" + filename from empty
            # TMPDIR, tries to create a file in /, and fails with
            # "Permission denied". env.TMPDIR doesn't work because
            # setup.sh runs after env attributes are applied.
            export TMPDIR="/tmp"

            # nix develop replaces PATH with the dev shell's packages,
            # but the nix-daemon.sh profile script uses a guard variable
            # (__ETC_PROFILE_NIX_SOURCED) to run only once per shell.
            # The guard survives into the subshell while the PATH it added
            # does not, so the base nix binary disappears. Re-add it.
            if ! command -v nix > /dev/null 2>&1; then
              export PATH="/nix/var/nix/profiles/default/bin:$PATH"
            fi

            # Install pre-commit hook trampoline. Global core.hooksPath
            # hooks in ~/.config/git/hooks/ cascade to .git/hooks/, so
            # we install here. script/pre-commit delegates to lefthook
            # which reads lefthook.yml. All formatting hooks run through
            # script/format-staged (index-only, working tree untouched).
            if [ -f script/pre-commit ] && [ -d .git ]; then
              hook=".git/hooks/pre-commit"
              mkdir -p "$(dirname "$hook")"
              expected='exec "$(git rev-parse --show-toplevel)/script/pre-commit" "$@"'
              if ! grep -qF "$expected" "$hook" 2>/dev/null; then
                printf '#!/bin/bash\n%s\n' "$expected" > "$hook"
                chmod +x "$hook"
              fi
            fi
          '';
        };
      }
    );
}
