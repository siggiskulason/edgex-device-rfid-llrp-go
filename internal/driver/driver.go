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
	"github.com/edgexfoundry/go-mod-bootstrap/bootstrap/flags"
	"github.com/edgexfoundry/go-mod-configuration/configuration"
	"github.com/edgexfoundry/go-mod-configuration/pkg/types"
	"github.com/edgexfoundry/go-mod-core-contracts/clients/logger"
	contract "github.com/edgexfoundry/go-mod-core-contracts/models"
	"github.com/pkg/errors"
	"github.impcloud.net/RSP-Inventory-Suite/device-llrp-go/internal/llrp"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	ServiceName    = "edgex-device-llrp"
	BaseConsulPath = "edgex/devices/1.0/" + ServiceName

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

// Driver manages a collection of devices that speak LLRP
// and connects them to the EdgeX ecosystem.
//
// A driver must be initialized before use.
// This is typically done by the Device Service SDK.
// This package maintains a package-global variable
// which it exports via driver.Instance.
//
// The Driver's exported methods are safe for concurrent use.
type Driver struct {
	lc       logger.LoggingClient
	asyncCh  chan<- *dsModels.AsyncValues
	deviceCh chan<- []dsModels.DiscoveredDevice

	done chan struct{}

	activeDevices map[string]*LLRPDevice
	devicesMu     sync.RWMutex

	config   *driverConfiguration
	configMu sync.RWMutex

	svc ServiceWrapper
}

// EdgeX's Device SDK takes an interface{}
// and uses a runtime-check to determine that it implements ProtocolDriver,
// at which point it will abruptly exit without a panic.
// This restores type-safety by making it so we can't compile
// unless we meet the runtime-required interface.
var _ dsModels.ProtocolDriver = (*Driver)(nil)

// Instance returns the package-global Driver instance, creating it if necessary.
// It must be initialized before use via its Initialize method.
func Instance() *Driver {
	createOnce.Do(func() {
		driver = &Driver{
			activeDevices: make(map[string]*LLRPDevice),
			done:          make(chan struct{}),
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
	d.svc = &DeviceSDKService{
		Service: service.RunningService(),
		lc:      lc,
	}

	config, err := CreateDriverConfig(d.svc.DriverConfigs())
	if err != nil && !errors.Is(err, ErrUnexpectedConfigItems) {
		return errors.Wrap(err, "read driver configuration failed")
	}

	d.configMu.Lock()
	d.config = config
	d.lc.Debug(fmt.Sprintf("%+v", config))
	d.configMu.Unlock()

	if err := d.watchForConfigChanges(); err != nil {
		d.lc.Warn("Unable to watch for configuration changes!", "error", err)
	}

	registered := d.svc.Devices()
	d.devicesMu.Lock()
	defer d.devicesMu.Unlock()
	for i := range registered {
		device := &registered[i] // the Device struct is nearly 1kb, so this avoids copying it

		addr, err := getAddr(device.Protocols)
		if err != nil {
			d.lc.Error("Unsupported protocol mapping.",
				"error", err,
				"protocols", fmt.Sprintf("%v", device.Protocols),
				"deviceName", device.Name)
			continue
		}

		d.lc.Info("Creating a new Reader connection.", "deviceName", device.Name)
		d.activeDevices[device.Name] = d.NewLLRPDevice(device.Name, addr)
	}

	return nil
}

func (d *Driver) watchForConfigChanges() error {
	sdkFlags := flags.New()
	sdkFlags.Parse(os.Args[1:])
	cpUrl, err := url.Parse(sdkFlags.ConfigProviderUrl())
	if err != nil {
		return err
	}

	cpPort := 8500
	port := cpUrl.Port()
	if port != "" {
		cpPort, err = strconv.Atoi(port)
		if err != nil {
			cpPort = 8500
		}
	}

	configClient, err := configuration.NewConfigurationClient(types.ServiceConfig{
		Host:     cpUrl.Hostname(),
		Port:     cpPort,
		BasePath: BaseConsulPath,
		Type:     cpUrl.Scheme,
	})
	if err != nil {
		return err
	}

	go func() {
		errorStream := make(chan error)
		defer close(errorStream)

		updateStream := make(chan interface{})
		defer close(updateStream)

		cfg := driverConfiguration{}
		configClient.WatchForChanges(updateStream, errorStream, &cfg, "/Driver")
		d.lc.Info("watching for configuration changes...")

		for {
			select {
			case <-d.done:
				return

			case ex := <-errorStream:
				d.lc.Error(ex.Error())

			case raw, ok := <-updateStream:
				if !ok {
					return
				}

				d.lc.Info("driver configuration has been updated!")
				d.lc.Debug(fmt.Sprintf("raw: %+v", raw))

				newCfg, ok := raw.(*driverConfiguration)
				if ok {
					d.configMu.Lock()
					d.config = newCfg
					d.configMu.Unlock()
				} else {
					d.lc.Warn("unable to decode incoming configuration from registry")
				}
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

	dev, _, err := d.getDevice(devName, p)
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

	dev, _, err := d.getDevice(devName, p)
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

// Stop the Driver, causing it to shutdown its active device connections
// and no longer process commands or upstream reports.
//
// If force is false, the Driver attempts to gracefully shutdown active devices
// by sending them a CloseConnection message and waiting a short time for their response.
// If force is true, it immediately closes all active connections.
// In neither case does it tell devices to stop reading.
//
// EdgeX says DeviceServices should close the async readings channel,
// but tracing their code reveals they call close on the channel,
// so doing so would cause a panic.
func (d *Driver) Stop(force bool) error {
	// Then Logging Client might not be initialized
	if d.lc == nil {
		d.lc = logger.NewClientStdOut(ServiceName, false, "DEBUG")
		d.lc.Error("EdgeX called Stop without calling Initialize >:(")
	}
	d.lc.Debug("LLRP-Driver.Stop called", "force", force)

	close(d.done)

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
			if err := dev.Stop(ctx); err != nil {
				d.lc.Error("Error attempting client shutdown.", "error", err.Error())
			}
			if !force {
				wg.Done()
			}
		}(dev)
	}

	d.activeDevices = make(map[string]*LLRPDevice)
	return nil
}

// AddDevice tells the driver attempt to actively manage a device.
//
// The Device Service SDK calls this when a new Device
// associated with this Device Service is added,
// so this assumes the device is already registered with EdgeX.
func (d *Driver) AddDevice(deviceName string, protocols protocolMap, adminState contract.AdminState) error {
	d.lc.Debug(fmt.Sprintf("Adding new device: %s protocols: %v adminState: %v",
		deviceName, protocols, adminState))
	_, _, err := d.getDevice(deviceName, protocols)
	if err != nil {
		d.lc.Error("Failed to add device.", "error", err, "deviceName", deviceName)
	}
	return err
}

// UpdateDevice updates a device managed by this Driver.
//
// The Device Service SDK calls it when a Device is updated,
// so this assumes the device is already registered with EdgeX.
//
// If the device's operating state is DISABLED,
// then if the Driver is not managing the device, nothing happens.
// If it is managing the device, it disconnects from it.
//
// If the device's operating state is ENABLED,
// if the Driver isn't currently managing a device with the given name,
// it'll create a new one and attempt to maintain its connection.
//
// If the Driver has a device with this name, but the device's address changes,
// this will shutdown any current connection associated with the named device,
// update the address, and attempt to reconnect at the new address and port.
// If the address is the same, nothing happens.
func (d *Driver) UpdateDevice(deviceName string, protocols protocolMap, adminState contract.AdminState) (err error) {
	d.lc.Debug(fmt.Sprintf("Updating device: %s protocols: %v adminState: %v",
		deviceName, protocols, adminState))
	defer func() {
		if err != nil {
			d.lc.Error("Failed to update device.",
				"error", err, "deviceName", deviceName,
				"protocolMap", fmt.Sprintf("%v", protocols))
		}
	}()

	// This uses the shutdownGrace period because updating the address
	// may require closing a current connection to an existing device.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	var dev *LLRPDevice
	var isNew bool
	dev, isNew, err = d.getDevice(deviceName, protocols)
	// No need to call update if the device was just created.
	if !(err == nil && isNew) {
		return err
	}

	var addr net.Addr
	addr, err = getAddr(protocols)
	if err != nil {
		return err
	}

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

// getDevice returns an LLRPDevice, creating one if needed.
//
// If the Driver is already managing an LLRPDevice with this name,
// then this call simply returns it.
// Otherwise, it creates and returns a new LLRPDevice instance
// after adding it to its map of managed devices.
// If the new LLRPDevice is created as a result of this call,
// the returned boolean `isNew` will be the true.
func (d *Driver) getDevice(name string, p protocolMap) (dev *LLRPDevice, isNew bool, err error) {
	// Try with just a read lock.
	d.devicesMu.RLock()
	c, ok := d.activeDevices[name]
	d.devicesMu.RUnlock()
	if ok {
		return c, false, nil
	}

	addr, err := getAddr(p)
	if err != nil {
		return nil, false, err
	}

	// It's important it holds the lock while creating a device.
	// If two requests arrive at about the same time and target the same device,
	// one will block waiting for the lock and the other will create/add it.
	// When gaining the lock, we recheck the map.
	// This way, only one device exists for any name,
	// and all requests that target it use the same one.
	d.devicesMu.Lock()
	defer d.devicesMu.Unlock()

	dev, ok = d.activeDevices[name]
	if ok {
		return dev, false, nil
	}

	d.lc.Info("Creating new connection for device.", "device", name)
	dev = d.NewLLRPDevice(name, addr)
	d.activeDevices[name] = dev
	return dev, true, nil
}

// removeDevice deletes a device from the active devices map
// and shuts down its client connection to an LLRP device.
func (d *Driver) removeDevice(ctx context.Context, deviceName string) {
	d.devicesMu.Lock()
	defer d.devicesMu.Unlock()

	if dev, ok := d.activeDevices[deviceName]; ok {
		d.lc.Info("Stopping connection for device.", "device", deviceName)
		if err := dev.Stop(ctx); err != nil {
			d.lc.Error("Error attempting client shutdown.", "error", err.Error())
		}
		delete(d.activeDevices, deviceName)
	}
}

// getAddr extracts an address from a protocol mapping.
//
// It expects the map to have {"tcp": {"host": "<ip>", "port": "<port>"}}.
func getAddr(protocols protocolMap) (net.Addr, error) {
	if protocols == nil {
		return nil, errors.New("protocol map is nil")
	}

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
	d.lc.Info("Discover was called.")

	d.configMu.RLock()
	maxSeconds := driver.config.MaxDiscoverDurationSeconds
	d.configMu.RUnlock()

	provisionOnce.Do(func() {
		err := d.addProvisionWatcher()
		if err != nil {
			d.lc.Error(err.Error())
			return
		}
	})

	ctx := context.Background()
	if maxSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(),
			time.Duration(maxSeconds)*time.Second)
		defer cancel()
	}

	d.discover(ctx)
}

func (d *Driver) discover(ctx context.Context) {
	d.configMu.RLock()
	params := discoverParams{
		subnets:    d.config.DiscoverySubnets,
		asyncLimit: d.config.ProbeAsyncLimit,
		timeout:    time.Duration(d.config.ProbeTimeoutSeconds) * time.Second,
		scanPort:   d.config.ScanPort,
	}
	d.configMu.RUnlock()

	t1 := time.Now()
	result := autoDiscover(ctx, params)
	if ctx.Err() != nil {
		d.lc.Warn("Discover process has been cancelled!", "ctxErr", ctx.Err())
	}
	d.deviceCh <- result
	d.lc.Info(fmt.Sprintf("Discovered %d new devices in %v.", len(result), time.Now().Sub(t1)))
}
