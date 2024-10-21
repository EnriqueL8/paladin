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

package pldclient

type Registry interface {
	RPCModule
}

// This is necessary because there's no way to introspect function parameter names via reflection
var registryInfo = &rpcModuleInfo{
	group:      "registry",
	methodInfo: map[string]RPCMethodInfo{},
}

type registry struct {
	*rpcModuleInfo
	c *paladinClient
}

func (c *paladinClient) Registry() Registry {
	return &registry{rpcModuleInfo: registryInfo, c: c}
}
