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

package registrymgr

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/kaleido-io/paladin/config/pkg/pldconf"
	"github.com/kaleido-io/paladin/core/internal/components"
	"github.com/kaleido-io/paladin/core/internal/msgs"

	"github.com/kaleido-io/paladin/toolkit/pkg/log"
	"github.com/kaleido-io/paladin/toolkit/pkg/prototk"
	"github.com/kaleido-io/paladin/toolkit/pkg/retry"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type registry struct {
	ctx       context.Context
	cancelCtx context.CancelFunc

	conf *pldconf.RegistryConfig
	rm   *registryManager
	id   uuid.UUID
	name string
	api  components.RegistryManagerToRegistry

	initialized atomic.Bool
	initRetry   *retry.Retry

	initError atomic.Pointer[error]
	initDone  chan struct{}
}

func (rm *registryManager) newRegistry(id uuid.UUID, name string, conf *pldconf.RegistryConfig, toRegistry components.RegistryManagerToRegistry) *registry {
	r := &registry{
		rm:        rm,
		conf:      conf,
		initRetry: retry.NewRetryIndefinite(&conf.Init.Retry),
		name:      name,
		id:        id,
		api:       toRegistry,
		initDone:  make(chan struct{}),
	}
	r.ctx, r.cancelCtx = context.WithCancel(log.WithLogField(rm.bgCtx, "registry", r.name))
	return r
}

func (r *registry) init() {
	defer close(r.initDone)

	// We block retrying each part of init until we succeed, or are cancelled
	// (which the plugin manager will do if the registry disconnects)
	err := r.initRetry.Do(r.ctx, func(attempt int) (bool, error) {
		// Send the configuration to the registry for processing
		confJSON, _ := json.Marshal(&r.conf.Config)
		_, err := r.api.ConfigureRegistry(r.ctx, &prototk.ConfigureRegistryRequest{
			Name:       r.name,
			ConfigJson: string(confJSON),
		})
		return true, err
	})
	if err != nil {
		log.L(r.ctx).Debugf("registry initialization cancelled before completion: %s", err)
		r.initError.Store(&err)
	} else {
		log.L(r.ctx).Debugf("registry initialization complete")
		r.initialized.Store(true)
		// Inform the plugin manager callback
		r.api.Initialized()
	}
}

func (r *registry) UpsertTransportDetails(ctx context.Context, req *prototk.UpsertTransportDetails) (*prototk.UpsertTransportDetailsResponse, error) {
	var postCommit func()
	err := r.rm.persistence.DB().Transaction(func(tx *gorm.DB) (err error) {
		postCommit, err = r.upsertTransportDetailsBatch(ctx, r.rm.persistence.DB(), req.TransportDetails)
		return err
	})
	if err != nil {
		return nil, err
	}
	postCommit()
	return &prototk.UpsertTransportDetailsResponse{}, nil
}

func (r *registry) upsertTransportDetailsBatch(ctx context.Context, dbTX *gorm.DB, protoEntries []*prototk.TransportDetails) (func(), error) {

	updatedNodes := make(map[string]bool)
	entries := make([]*components.RegistryNodeTransportEntry, len(protoEntries))
	for i, req := range protoEntries {
		if req.Node == "" || req.Transport == "" {
			return nil, i18n.NewError(ctx, msgs.MsgRegistryInvalidEntry)
		}
		updatedNodes[req.Node] = true
		entries[i] = &components.RegistryNodeTransportEntry{
			Registry:  r.id.String(),
			Node:      req.Node,
			Transport: req.Transport,
			Details:   req.Details,
		}
	}

	// Store entry in database
	err := dbTX.
		WithContext(ctx).
		Table("registry_transport_details").
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "registry"},
				{Name: "node"},
				{Name: "transport"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"details", // we replace any existing entry
			}),
		}).
		Create(entries).
		Error
	if err != nil {
		return nil, err
	}

	// return a post-commit callback to update the cache
	return func() {
		for node := range updatedNodes {
			// The cache is by node, and we only have complete entries - so just invalid the cache
			r.rm.registryCache.Delete(node)
		}
	}, nil
}

func (r *registry) close() {
	r.cancelCtx()
	<-r.initDone
}
