#!/usr/bin/env bash
# Copyright 2026 The Bureau Authors
# SPDX-License-Identifier: Apache-2.0
#
# Verifies Lean 4 formal proofs of Bureau's reconciliation loop.
# Runs lake build in a writable copy of the project (lake writes
# .lake/ build artifacts). All proofs must verify with zero sorries.

set -euo pipefail

# Resolve the Lean toolchain root from the marker file written by the
# lean_toolchain repository rule. The marker contains the absolute path
# to the extracted toolchain (bin/lake, bin/lean, lib/lean/).
toolchain_root_file="${RUNFILES_DIR}/${LEAN_TOOLCHAIN_ROOT}"
if [ ! -f "${toolchain_root_file}" ]; then
    echo "ERROR: toolchain_root file not found at ${toolchain_root_file}"
    echo "Run: bazel fetch @lean4//:toolchain_root"
    exit 1
fi

toolchain_root=$(cat "${toolchain_root_file}")
lean_bin="${toolchain_root}/bin"
if [ ! -x "${lean_bin}/lake" ]; then
    echo "ERROR: lake not found at ${lean_bin}/lake"
    echo "The Lean toolchain may not have been extracted correctly."
    exit 1
fi
export PATH="${lean_bin}:${PATH}"

# Lake writes .lake/ build artifacts, so copy the project to a writable
# tree. Runfiles may be symlinks; -L resolves them.
source_directory="${RUNFILES_DIR}/${TEST_WORKSPACE}/verify/reconciliation"
work_directory="${TEST_TMPDIR}/reconciliation"
cp -rL "${source_directory}" "${work_directory}"
cd "${work_directory}"

# Verify proofs. lake build compiles all .lean files, checking every
# theorem and definition. Any sorry (unproven goal) or type error fails
# the build.
lake build

echo "All Lean proofs verified (zero sorries)."
