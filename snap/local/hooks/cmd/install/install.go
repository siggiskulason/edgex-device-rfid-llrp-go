// -*- Mode: Go; indent-tabs-mode: t -*-

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

package main

import (
	"fmt"
	"os"

	hooks "github.com/canonical/edgex-snap-hooks/v2"
)

var cli *hooks.CtlCli = hooks.NewSnapCtl()

// installProfiles copies the profile configuration.toml files from $SNAP to $SNAP_DATA.
func installConfig() error {
	var err error

	if err = os.MkdirAll(hooks.SnapDataConf+"/device-rfid-llrp/res/provision_watchers", 0755); err != nil {
		return err
	}

	files := []string{
		"configuration.toml",
		"llrp.device.profile.yaml",
		"llrp.impinj.profile.yaml",
		"provision_watchers/impinj.provision.watcher.json",
		"provision_watchers/llrp.provision.watcher.json",
	}

	srcDir := hooks.SnapConf + "/device-rfid-llrp/res/"
	destDir := hooks.SnapDataConf + "/device-rfid-llrp/res/"

	for _, fileName := range files {
		srcPath := srcDir + fileName
		destPath := destDir + fileName
		if err = hooks.CopyFile(srcPath, destPath); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	var err error

	if err = hooks.Init(false, "edgex-device-rfid-llrp"); err != nil {
		fmt.Println(fmt.Sprintf("edgex-device-rfid-llrp::install: initialization failure: %v", err))
		os.Exit(1)

	}

	err = installConfig()
	if err != nil {
		hooks.Error(fmt.Sprintf("edgex-device-rfid-llrp:install: %v", err))
		os.Exit(1)
	}

}
