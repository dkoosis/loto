#!/usr/bin/env bash
# go-sandbox lib-env.sh — the ONE Go environment for a sandboxed repo.
#
# Sourced by BOTH lib-activate.sh (task time) and lib-setup.sh (container
# build). One file, two consumers, so setup warms exactly the caches that
# activation later reads.
#
# Why it exists: these used to be two separate env blocks, and they disagreed.
# A Codex container ran setup under the ambient Go environment, warming the
# module and build caches there, and then `source .sandbox/activate.sh`
# repointed GOCACHE/GOMODCACHE at empty repo-local directories. By task time
# egress is cut, so every build started cold and the toolchain go.mod requires
# was unreachable — with the warm copy sitting one directory away.
#
# Requires: nothing. Safe to source more than once.

# GOTOOLCHAIN=auto, not local. `local` pins Go to whatever the base image
# ships, so a go.mod requiring a newer release becomes a hard version error the
# moment this file is sourced. `auto` lets setup fetch the required toolchain
# into the module cache while the network is up; task time finds it there.
export GOTOOLCHAIN=auto
export GOPROXY="https://proxy.golang.org,direct"
export GOSUMDB="sum.golang.org"

# Go's own cache locations — ambient and per-machine — are the choice here, not
# an omission. They are shared by every repo in the fleet and they are what the
# container image snapshot already carries. Repo-local caches bought no
# isolation and cost roughly 9.5 GB of duplicated module and build cache across
# nine checkouts.
#
# Unset only the old repo-local scheme, so a shell that sourced the previous
# activate.sh heals instead of staying pointed at an empty cache. An operator's
# own GOCACHE elsewhere is left alone.
case "${GOCACHE:-}" in */.sandbox/cache/*) unset GOCACHE ;; esac
case "${GOMODCACHE:-}" in */.sandbox/cache/*) unset GOMODCACHE ;; esac
case "${GOLANGCI_LINT_CACHE:-}" in */.sandbox/cache/*) unset GOLANGCI_LINT_CACHE ;; esac

GOMAXPROCS=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
export GOMAXPROCS
