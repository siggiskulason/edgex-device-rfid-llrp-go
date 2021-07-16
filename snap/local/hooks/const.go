/*
 * Copyright (C) 2021 Canonical Ltd
 *
 *  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
 *  in compliance with the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License
 * is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
 * or implied. See the License for the specific language governing permissions and limitations under
 * the License.
 *
 * SPDX-License-Identifier: Apache-2.0'
 */

package hooks

// ConfToEnv defines mappings from snap config keys to EdgeX environment variable
// names that are used to override individual device-rest's [Device]  configuration
// values via a .env file read by the snap service wrapper.
//
// The syntax to set a configuration key is:
//
// env.<section>.<keyname>
//
var ConfToEnv = map[string]string{
	// [Driver]

	// List of IPv4 subnets to perform LLRP discovery process on, in CIDR format (X.X.X.X/Y)
	// separated by commas ex: "192.168.1.0/24,10.0.0.0/24"
	"driver.discovery-subnets": "DRIVER_DISCOVERYSUBNETS",
	// Maximum simultaneous network probes
	"driver.probe-async-limit": "DRIVER_PROBEASYNCLIMIT",

	// Maximum amount of seconds to wait for each IP probe before timing out.
	// This will also be the minimum time the discovery process can take.
	"driver.probe-timeout-seconds": "DRIVER_PROBETIMEOUTSECONDS",

	// Port to scan for LLRP devices on
	"driver.scan-port": "DRIVER_SCANPORT",

	// Maximum amount of seconds the discovery process is allowed to run before it will be cancelled.
	// It is especially important to have this configured in the case of larger subnets such as /16 and /8
	"driver.max-discover-duration-seconds": "DRIVER_MAXDISCOVERYDURATIONSECONDS",
}
