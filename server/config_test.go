// Copyright 2026 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Dockerfile copies this file to /secrets/policy-bot.yml and the server runs with it,
// so a typo here is only found at startup.
func TestBundledConfigParses(t *testing.T) {
	bytes, err := os.ReadFile("../config/policy-bot.example.yml")
	require.NoError(t, err)

	c, err := ParseConfig(bytes)
	require.NoError(t, err)

	assert.Equal(t, 1000, c.Workers.QueueSize)
}
