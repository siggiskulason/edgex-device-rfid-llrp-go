//
// Copyright (C) 2020 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	dsModels "github.com/edgexfoundry/device-sdk-go/pkg/models"
	"github.com/edgexfoundry/device-sdk-go/pkg/service"
	"github.com/edgexfoundry/go-mod-core-contracts/clients/logger"
	contract "github.com/edgexfoundry/go-mod-core-contracts/models"
	"github.com/pkg/errors"
	"github.impcloud.net/RSP-Inventory-Suite/device-llrp-go/internal/llrp"
	"io/ioutil"
	"net"
	"sync"
	"time"
)

const (
	ServiceName = "edgex-device-llrp"

	ResourceReaderCap          = "ReaderCapabilities"
	ResourceReaderConfig       = "ReaderConfig"
	ResourceReaderNotification = "ReaderEventNotification"
	ResourceROSpec             = "ROSpec"
	ResourceROSpecID           = "ROSpecID"
	ResourceAccessSpec         = "AccessSpec"
	ResourceAccessSpecID       = "AccessSpecID"
	ResourceROAccessReport     = "ROAccessReport"

	ResourceAction = "Action"
	ActionDelete   = "Delete"
	ActionEnable   = "Enable"
	ActionDisable  = "Disable"
	ActionStart    = "Start"
	ActionStop     = "Stop"

	provisionWatcherFilename = "res/provisionwatcher.json"
)

var (
	createOnce    sync.Once
	provisionOnce sync.Once
	driver        *Driver
)

type protocolMap = map[string]contract.ProtocolProperties

type Driver struct {
	lc       logger.LoggingClient
	asyncCh  chan<- *dsModels.AsyncValues
	deviceCh chan<- []dsModels.DiscoveredDevice

	activeDevices map[string]*LLRPDevice
	devicesMu     sync.RWMutex

	config *driverConfiguration

	svc ServiceWrapper
}

func NewProtocolDriver() dsModels.ProtocolDriver {
	createOnce.Do(func() {
		driver = &Driver{
			activeDevices: make(map[string]*LLRPDevice),
		}
	})
	return driver
}

// Initialize performs protocol-specific initialization for the device
// service.
func (d *Driver) Initialize(lc logger.LoggingClient, asyncCh chan<- *dsModels.AsyncValues, deviceCh chan<- []dsModels.DiscoveredDevice) error {
	if lc == nil {
		// prevent panics from this annoyance
		d.lc = logger.NewClientStdOut(ServiceName, false, "DEBUG")
		d.lc.Error("EdgeX initialized us with a nil logger >:(")
	} else {
		d.lc = lc
	}

	d.asyncCh = asyncCh
	d.deviceCh = deviceCh
	d.svc = &DeviceSDKService{service.RunningService()}

	config, err := CreateDriverConfig(d.svc.DriverConfigs())
	if err != nil {
		return errors.Wrap(err, "read driver configuration failed")
	}

	d.config = config
	d.lc.Debug(fmt.Sprintf("%+v", config))

	go func() {
		// todo: how to know when this is ready
		time.Sleep(5 * time.Second)

		// startup all devices
		for _, dev := range d.svc.Devices() {
			if _, err := d.getDevice(dev.Name, dev.Protocols); err != nil {
				d.lc.Error(err.Error())
			}
		}
	}()

	return nil
}

// HandleReadCommands triggers a protocol Read operation for the specified device.
func (d *Driver) HandleReadCommands(devName string, p protocolMap, reqs []dsModels.CommandRequest) ([]*dsModels.CommandValue, error) {
	d.lc.Debug(fmt.Sprintf("LLRP-Driver.HandleReadCommands: "+
		"device: %s protocols: %v reqs: %+v", devName, p, reqs))

	results, err := d.handleReadCommands(devName, p, reqs)
	if err != nil {
		d.lc.Error("ReadCommands failed.",
			"device", devName,
			"error", err,
			"requests", fmt.Sprintf("%+v", reqs))
	}
	return results, err
}

func (d *Driver) handleReadCommands(devName string, p protocolMap, reqs []dsModels.CommandRequest) ([]*dsModels.CommandValue, error) {
	if len(reqs) == 0 {
		return nil, errors.New("missing requests")
	}

	dev, err := d.getDevice(devName, p)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	var responses = make([]*dsModels.CommandValue, len(reqs))
	for i := range reqs {
		var llrpReq llrp.Outgoing
		var llrpResp llrp.Incoming

		switch reqs[i].DeviceResourceName {
		case ResourceReaderConfig:
			llrpReq = &llrp.GetReaderConfig{}
			llrpResp = &llrp.GetReaderConfigResponse{}
		case ResourceReaderCap:
			llrpReq = &llrp.GetReaderCapabilities{}
			llrpResp = &llrp.GetReaderCapabilitiesResponse{}
		case ResourceROSpec:
			llrpReq = &llrp.GetROSpecs{}
			llrpResp = &llrp.GetROSpecsResponse{}
		case ResourceAccessSpec:
			llrpReq = &llrp.GetAccessSpecs{}
			llrpResp = &llrp.GetAccessSpecsResponse{}
		}

		if err := dev.TrySend(ctx, llrpReq, llrpResp); err != nil {
			return nil, err
		}

		respData, err := json.Marshal(llrpResp)
		if err != nil {
			return nil, err
		}

		responses[i] = dsModels.NewStringValue(
			reqs[i].DeviceResourceName, time.Now().UnixNano(), string(respData))
	}

	return responses, nil
}

// HandleWriteCommands passes a slice of CommandRequest struct each representing
// a ResourceOperation for a specific device resource.
// Since the commands are actuation commands, params provide parameters for the individual
// command.
func (d *Driver) HandleWriteCommands(devName string, p protocolMap, reqs []dsModels.CommandRequest, params []*dsModels.CommandValue) error {
	d.lc.Debug(fmt.Sprintf("LLRP-Driver.HandleWriteCommands: "+
		"device: %s protocols: %v reqs: %+v", devName, p, reqs))

	// kinda surprised EdgeX doesn't do this automatically.
	err := d.handleWriteCommands(devName, p, reqs, params)
	if err != nil {
		d.lc.Error("Write Command failed",
			"device", devName,
			"error", err.Error())
	}
	return err
}

func (d *Driver) handleWriteCommands(devName string, p protocolMap, reqs []dsModels.CommandRequest, params []*dsModels.CommandValue) error {
	if len(reqs) == 0 {
		return errors.New("missing requests")
	}

	dev, err := d.getDevice(devName, p)
	if err != nil {
		return err
	}

	getParam := func(name string, idx int, key string) (*dsModels.CommandValue, error) {
		if idx > len(params) {
			return nil, errors.Errorf("%s needs at least %d parameters, but got %d",
				name, idx, len(params))
		}

		cv := params[idx]
		if cv == nil {
			return nil, errors.Errorf("%s requires parameter %s", name, key)
		}

		if cv.DeviceResourceName != key {
			return nil, errors.Errorf("%s expected parameter %d: %s, but got %s",
				name, idx, key, cv.DeviceResourceName)
		}

		return cv, nil
	}

	getStrParam := func(name string, idx int, key string) (string, error) {
		if cv, err := getParam(name, idx, key); err != nil {
			return "", err
		} else {
			return cv.StringValue()
		}
	}

	getUint32Param := func(name string, idx int, key string) (uint32, error) {
		if cv, err := getParam(name, idx, key); err != nil {
			return 0, err
		} else {
			return cv.Uint32Value()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	var llrpReq llrp.Outgoing  // the message to send
	var llrpResp llrp.Incoming // the expected response
	var reqData []byte         // incoming JSON request data, if present
	var dataTarget interface{} // used if the reqData in a subfield of the llrpReq

	switch reqs[0].DeviceResourceName {
	case ResourceReaderConfig:
		data, err := getStrParam("Set"+ResourceReaderConfig, 0, ResourceReaderConfig)
		if err != nil {
			return err
		}

		reqData = []byte(data)
		llrpReq = &llrp.SetReaderConfig{}
		llrpResp = &llrp.SetReaderConfigResponse{}
	case ResourceROSpec:
		data, err := getStrParam("Add"+ResourceROSpec, 0, ResourceROSpec)
		if err != nil {
			return err
		}

		reqData = []byte(data)

		addSpec := llrp.AddROSpec{}
		dataTarget = &addSpec.ROSpec // the incoming data is an ROSpec, not AddROSpec
		llrpReq = &addSpec           // but we want to send AddROSpec, not just ROSpec
		llrpResp = &llrp.AddROSpecResponse{}
	case ResourceROSpecID:
		if len(params) != 2 {
			return errors.Errorf("expected 2 resources for ROSpecID op, but got %d", len(params))
		}

		action, err := getStrParam(ResourceROSpec, 1, ResourceAction)
		if err != nil {
			return err
		}

		roID, err := getUint32Param(action+ResourceROSpec, 0, ResourceROSpecID)
		if err != nil {
			return err
		}

		switch action {
		default:
			return errors.Errorf("unknown ROSpecID action: %q", action)
		case ActionEnable:
			llrpReq = &llrp.EnableROSpec{ROSpecID: roID}
			llrpResp = &llrp.EnableROSpecResponse{}
		case ActionStart:
			llrpReq = &llrp.StartROSpec{ROSpecID: roID}
			llrpResp = &llrp.StartROSpecResponse{}
		case ActionStop:
			llrpReq = &llrp.StopROSpec{ROSpecID: roID}
			llrpResp = &llrp.StopROSpecResponse{}
		case ActionDisable:
			llrpReq = &llrp.DisableROSpec{ROSpecID: roID}
			llrpResp = &llrp.DisableROSpecResponse{}
		case ActionDelete:
			llrpReq = &llrp.DeleteROSpec{ROSpecID: roID}
			llrpResp = &llrp.DeleteROSpecResponse{}
		}

	case ResourceAccessSpecID:
		if len(reqs) != 2 {
			return errors.Errorf("expected 2 resources for AccessSpecID op, but got %d", len(reqs))
		}

		action := reqs[1].DeviceResourceName

		asID, err := getUint32Param(action+ResourceAccessSpecID, 0, ResourceAccessSpecID)
		if err != nil {
			return err
		}

		switch action {
		default:
			return errors.Errorf("unknown ROSpecID action: %q", action)
		case ActionEnable:
			llrpReq = &llrp.EnableAccessSpec{AccessSpecID: asID}
			llrpResp = &llrp.EnableAccessSpecResponse{}
		case ActionDisable:
			llrpReq = &llrp.DisableAccessSpec{AccessSpecID: asID}
			llrpResp = &llrp.DisableAccessSpecResponse{}
		case ActionDelete:
			llrpReq = &llrp.DeleteAccessSpec{AccessSpecID: asID}
			llrpResp = &llrp.DeleteAccessSpecResponse{}
		}
	}

	if reqData != nil {
		if dataTarget != nil {
			if err := json.Unmarshal(reqData, dataTarget); err != nil {
				return errors.Wrap(err, "failed to unmarshal request")
			}
		} else {
			if err := json.Unmarshal(reqData, llrpReq); err != nil {
				return errors.Wrap(err, "failed to unmarshal request")
			}
		}
	}

	// SendFor will handle turning ErrorMessages and failing LLRPStatuses into errors.
	if err := dev.TrySend(ctx, llrpReq, llrpResp); err != nil {
		return err
	}

	go func(resName, devName string, resp llrp.Incoming) {
		respData, err := json.Marshal(resp)
		if err != nil {
			d.lc.Error("failed to marshal response", "message", resName, "error", err)
			return
		}

		cv := dsModels.NewStringValue(resName, time.Now().UnixNano(), string(respData))
		d.asyncCh <- &dsModels.AsyncValues{
			DeviceName:    devName,
			CommandValues: []*dsModels.CommandValue{cv},
		}
	}(reqs[0].DeviceResourceName, dev.name, llrpResp)

	return nil
}

// Stop the protocol-specific DS code to shutdown gracefully, or
// if the force parameter is 'true', immediately. The driver is responsible
// for closing any in-use channels, including the channel used to send async
// readings (if supported).
func (d *Driver) Stop(force bool) error {
	// Then Logging Client might not be initialized
	if d.lc == nil {
		d.lc = logger.NewClientStdOut(ServiceName, false, "DEBUG")
		d.lc.Error("EdgeX called Stop without calling Initialize >:(")
	}
	d.lc.Debug("LLRP-Driver.Stop called", "force", force)

	d.devicesMu.Lock()
	defer d.devicesMu.Unlock()

	ctx := context.Background()

	var wg *sync.WaitGroup
	if !force {
		wg = new(sync.WaitGroup)
		wg.Add(len(d.activeDevices))
		defer wg.Wait()

		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownGrace)
		defer cancel()
	}

	for _, dev := range d.activeDevices {
		go func(dev *LLRPDevice) {
			d.stopDevice(ctx, dev)
			if !force {
				wg.Done()
			}
		}(dev)
	}

	d.activeDevices = make(map[string]*LLRPDevice)
	return nil
}

// AddDevice is a callback function that is invoked
// when a new Device associated with this Device Service is added
func (d *Driver) AddDevice(deviceName string, protocols protocolMap, adminState contract.AdminState) error {
	d.lc.Debug(fmt.Sprintf("Adding new device: %s protocols: %v adminState: %v",
		deviceName, protocols, adminState))

	_, err := d.getDevice(deviceName, protocols)
	return err
}

// UpdateDevice is a callback function that is invoked
// when a Device associated with this Device Service is updated
func (d *Driver) UpdateDevice(deviceName string, protocols protocolMap, adminState contract.AdminState) error {
	d.lc.Debug(fmt.Sprintf("Updating device: %s protocols: %v adminState: %v",
		deviceName, protocols, adminState))

	dev, err := d.getDevice(deviceName, protocols)
	if err != nil {
		return err
	}

	addr, err := getAddr(protocols)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return dev.UpdateAddr(ctx, addr)
}

// RemoveDevice is a callback function that is invoked
// when a Device associated with this Device Service is removed
func (d *Driver) RemoveDevice(deviceName string, p protocolMap) error {
	d.lc.Debug(fmt.Sprintf("Removing device: %s protocols: %v", deviceName, p))

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	d.removeDevice(ctx, deviceName)
	return nil
}

// getOrCreate returns a Client, creating one if needed.
//
// If a Client with this name already exists, it returns it.
// Otherwise, calls the createNew function to get a new Client,
// which it adds to the map and then returns.
func (d *Driver) getDevice(name string, p protocolMap) (*LLRPDevice, error) {
	// Try with just a read lock.
	d.devicesMu.RLock()
	c, ok := d.activeDevices[name]
	d.devicesMu.RUnlock()
	if ok {
		return c, nil
	}

	addr, err := getAddr(p)
	if err != nil {
		return nil, err
	}

	// It's important it holds the lock while creating a device.
	// If two requests arrive at about the same time and target the same device,
	// one will block waiting for the lock and the other will create/add it.
	// When gaining the lock, we recheck the map
	// This way, only one device exists for any name,
	// and all requests that target it use the same one.
	d.devicesMu.Lock()
	defer d.devicesMu.Unlock()

	dev, ok := d.activeDevices[name]
	if ok {
		return dev, nil
	}

	d.lc.Info("Creating new connection for device.", "device", name)
	dev = d.NewLLRPDevice(name, addr)
	d.activeDevices[name] = dev
	return dev, nil
}

// removeDevice deletes a device from the active devices map
// and shuts down its client connection to an LLRP device.
func (d *Driver) removeDevice(ctx context.Context, deviceName string) {
	d.devicesMu.Lock()
	defer d.devicesMu.Unlock()

	if dev, ok := d.activeDevices[deviceName]; ok {
		d.lc.Info("Stopping connection for device.", "device", deviceName)
		go d.stopDevice(ctx, dev)
		delete(d.activeDevices, deviceName)
	}
}

// stopDevice stops a device's reconnect loop,
// closing any active connection it may currently have.
// Any pending requests targeting that device may fail.
// This doesn't remove it from the devices map.
func (d *Driver) stopDevice(ctx context.Context, dev *LLRPDevice) {
	if err := dev.Stop(ctx); err != nil {
		d.lc.Error("Error attempting client shutdown.", "error", err.Error())
	}
}

// getAddr extracts an address from a protocol mapping.
//
// It expects the map to have {"tcp": {"host": "<ip>", "port": "<port>"}}.
func getAddr(protocols protocolMap) (net.Addr, error) {
	tcpInfo := protocols["tcp"]
	if tcpInfo == nil {
		return nil, errors.New("missing tcp protocol")
	}

	host, port := tcpInfo["host"], tcpInfo["port"]
	if host == "" || port == "" {
		return nil, errors.Errorf("tcp missing host or port (%q, %q)", host, port)
	}

	addr, err := net.ResolveTCPAddr("tcp", host+":"+port)
	return addr, errors.Wrapf(err,
		"unable to create addr for tcp protocol (%q, %q)", host, port)
}

func (d *Driver) addProvisionWatcher() error {
	var provisionWatcher contract.ProvisionWatcher
	data, err := ioutil.ReadFile(provisionWatcherFilename)
	if err != nil {
		return err
	}

	err = provisionWatcher.UnmarshalJSON(data)
	if err != nil {
		return err
	}

	if err := d.svc.AddOrUpdateProvisionWatcher(provisionWatcher); err != nil {
		return err
	}

	return nil
}

// Discover performs a discovery of LLRP readers on the network and passes them to EdgeX to get provisioned
func (d *Driver) Discover() {
	d.lc.Info("discover was called")

	provisionOnce.Do(func() {
		err := d.addProvisionWatcher()
		if err != nil {
			d.lc.Error(err.Error())
			return
		}
	})

	t1 := time.Now()
	result := autoDiscover()
	d.deviceCh <- result
	d.lc.Info(fmt.Sprintf("discovered %d new devices in %v", len(result), time.Now().Sub(t1)))
}
