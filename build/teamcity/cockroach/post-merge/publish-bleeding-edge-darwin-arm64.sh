#!/usr/bin/env bash

# Copyright 2023 The Cockroach Authors.
#
# Use of this software is governed by the CockroachDB Software License
# included in the /LICENSE file.


PLATFORM=darwin-arm64 ./build/teamcity/cockroach/post-merge/publish-bleeding-edge-per-platform.sh
