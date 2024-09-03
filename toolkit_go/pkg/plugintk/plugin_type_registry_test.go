/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */
package plugintk

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/kaleido-io/paladin/toolkit/pkg/prototk"
	"github.com/stretchr/testify/assert"
)

func setupRegistryTests(t *testing.T) (context.Context, *pluginExerciser[prototk.RegistryMessage], *RegistryAPIFunctions, RegistryCallbacks, map[string]func(*prototk.RegistryMessage), func()) {
	ctx, tc, tcDone := newTestController(t)

	/***** THIS PART AN IMPLEMENTATION WOULD DO ******/
	funcs := &RegistryAPIFunctions{
		// Functions go here
	}
	waitForCallbacks := make(chan RegistryCallbacks, 1)
	registry := NewRegistry(func(callbacks RegistryCallbacks) RegistryAPI {
		// Implementation would construct an instance here to start handling the API calls from Paladin,
		// (rather than passing the callbacks to the test as we do here)
		waitForCallbacks <- callbacks
		return &RegistryAPIBase{funcs}
	})
	/************************************************/

	// The rest is mocking the other side of the interface
	inOutMap := map[string]func(*prototk.RegistryMessage){}
	pluginID := uuid.NewString()
	exerciser := newPluginExerciser(t, pluginID, &RegistryMessageWrapper{}, inOutMap)
	tc.fakeRegistryController = exerciser.controller

	registryDone := make(chan struct{})
	go func() {
		defer close(registryDone)
		registry.Run("unix:"+tc.socketFile, pluginID)
	}()
	callbacks := <-waitForCallbacks

	return ctx, exerciser, funcs, callbacks, inOutMap, func() {
		checkPanic()
		registry.Stop()
		tcDone()
		<-registryDone
	}
}

func TestRegistryFunction_ConfigureRegistry(t *testing.T) {
	_, exerciser, funcs, _, _, done := setupRegistryTests(t)
	defer done()

	// ConfigureRegistry - paladin to registry
	funcs.ConfigureRegistry = func(ctx context.Context, cdr *prototk.ConfigureRegistryRequest) (*prototk.ConfigureRegistryResponse, error) {
		return &prototk.ConfigureRegistryResponse{}, nil
	}
	exerciser.doExchangeToPlugin(func(req *prototk.RegistryMessage) {
		req.RequestToRegistry = &prototk.RegistryMessage_ConfigureRegistry{
			ConfigureRegistry: &prototk.ConfigureRegistryRequest{},
		}
	}, func(res *prototk.RegistryMessage) {
		assert.IsType(t, &prototk.RegistryMessage_ConfigureRegistryRes{}, res.ResponseFromRegistry)
	})
}

func TestRegistryFunction_ResolveTransportDetails(t *testing.T) {
	_, exerciser, funcs, _, _, done := setupRegistryTests(t)
	defer done()

	// InitRegistry - paladin to registry
	funcs.ResolveTransportDetails = func(ctx context.Context, cdr *prototk.ResolveTransportDetailsRequest) (*prototk.ResolveTransportDetailsResponse, error) {
		return &prototk.ResolveTransportDetailsResponse{}, nil
	}
	exerciser.doExchangeToPlugin(func(req *prototk.RegistryMessage) {
		req.RequestToRegistry = &prototk.RegistryMessage_ResolveTransportDetails{
			ResolveTransportDetails: &prototk.ResolveTransportDetailsRequest{},
		}
	}, func(res *prototk.RegistryMessage) {
		assert.IsType(t, &prototk.RegistryMessage_ResolveTransportDetailsRes{}, res.ResponseFromRegistry)
	})
}

func TestRegistryRequestError(t *testing.T) {
	_, exerciser, _, _, _, done := setupRegistryTests(t)
	defer done()

	// Check responseToPluginAs handles nil
	exerciser.doExchangeToPlugin(func(req *prototk.RegistryMessage) {}, func(res *prototk.RegistryMessage) {
		assert.Regexp(t, "PD020300", *res.Header.ErrorMessage)
	})
}
