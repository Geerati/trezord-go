package usb

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/trezor/trezord-go/usb/lowlevel"

	"github.com/trezor/trezord-go/core"
	"github.com/trezor/trezord-go/memorywriter"
)

const (
	webusbPrefix  = "web"
	webConfigNum  = 1
	webIfaceNum   = 0
	webAltSetting = 0
	webEpIn       = 0x81
	webEpOut      = 0x01
)

type WebUSB struct {
	usb    lowlevel.Context
	mw     *memorywriter.MemoryWriter
	only   bool
	cancel bool
}

func InitWebUSB(mw *memorywriter.MemoryWriter, onlyLibusb, allowCancel bool) (*WebUSB, error) {
	var usb lowlevel.Context
	mw.Println("webusb - init")
	lowlevel.SetLogWriter(mw)

	err := lowlevel.Init(&usb)
	if err != nil {
		return nil, err
	}

	mw.Println("webusb - init done")

	return &WebUSB{
		usb:    usb,
		mw:     mw,
		only:   onlyLibusb,
		cancel: allowCancel,
	}, nil
}

func (b *WebUSB) Close() {
	b.mw.Println("webusb - all close (should happen only on exit)")
	lowlevel.Exit(b.usb)
}

func (b *WebUSB) Enumerate() ([]core.USBInfo, error) {
	b.mw.Println("webusb - enum - low level enumerating")
	list, err := lowlevel.Get_Device_List(b.usb)

	if err != nil {
		return nil, err
	}
	b.mw.Println("webusb - enum - low level enumerating done")

	defer func() {
		b.mw.Println("webusb - enum - freeing device list")
		lowlevel.Free_Device_List(list, 1) // unlink devices
		b.mw.Println("webusb - enum - freeing device list done")
	}()

	var infos []core.USBInfo

	// There is a bug in libusb that makes
	// device appear twice with the same path.
	// This is already fixed in libusb 2.0.12;
	// however, 2.0.12 has other problems with windows, so we
	// patchfix it here
	paths := make(map[string]bool)

	for _, dev := range list {
		m, t := b.match(dev)
		if m {
			b.mw.Println("webusb - enum - getting device descriptor")
			dd, err := lowlevel.Get_Device_Descriptor(dev)
			if err != nil {
				b.mw.Println("webusb - enum - error getting device descriptor " + err.Error())
				continue
			}
			path := b.identify(dev)
			inset := paths[path]
			if !inset {
				infos = append(infos, core.USBInfo{
					Path:      path,
					VendorID:  int(dd.IdVendor),
					ProductID: int(dd.IdProduct),
					Type:      t,
				})
				paths[path] = true
			}
		}
	}
	return infos, nil
}

func (b *WebUSB) Has(path string) bool {
	return strings.HasPrefix(path, webusbPrefix)
}

func (b *WebUSB) Connect(path string) (core.USBDevice, error) {
	b.mw.Println("webusb - connect - low level enumerating")
	list, err := lowlevel.Get_Device_List(b.usb)

	if err != nil {
		return nil, err
	}
	b.mw.Println("webusb - connect - low level enumerating done")

	defer func() {
		b.mw.Println("webusb - connect - freeing device list")
		lowlevel.Free_Device_List(list, 1) // unlink devices
		b.mw.Println("webusb - connect - freeing device list done")
	}()

	// There is a bug in libusb that makes
	// device appear twice with the same path.
	// This is already fixed in libusb 2.0.12;
	// however, 2.0.12 has other problems with windows, so we
	// patchfix it here
	mydevs := make([]lowlevel.Device, 0)
	for _, dev := range list {
		m, _ := b.match(dev)
		if m && b.identify(dev) == path {
			mydevs = append(mydevs, dev)
		}
	}

	err = ErrNotFound
	for _, dev := range mydevs {
		res, errConn := b.connect(dev)
		if errConn == nil {
			return res, nil
		}
		err = errConn
	}
	return nil, err
}

func (b *WebUSB) connect(dev lowlevel.Device) (*WUD, error) {
	b.mw.Println("webusb - connect - low level")
	d, err := lowlevel.Open(dev)
	if err != nil {
		return nil, err
	}
	b.mw.Println("webusb - connect - reset")
	err = lowlevel.Reset_Device(d)
	if err != nil {
		// don't abort if reset fails
		// lowlevel.Close(d)
		// return nil, err
		b.mw.Println(fmt.Sprintf("Warning: error at device reset: %s", err))
	}

	currConf, err := lowlevel.Get_Configuration(d)
	if err != nil {
		b.mw.Println(fmt.Sprintf("webusb - connect - current configuration err %s", err.Error()))
	} else {
		b.mw.Println(fmt.Sprintf("webusb - connect - current configuration %d", currConf))
	}

	b.mw.Println("webusb - connect - set_configuration")
	err = lowlevel.Set_Configuration(d, webConfigNum)
	if err != nil {
		// don't abort if set configuration fails
		// lowlevel.Close(d)
		// return nil, err
		b.mw.Println(fmt.Sprintf("Warning: error at configuration set: %s", err))
	}

	currConf, err = lowlevel.Get_Configuration(d)
	if err != nil {
		b.mw.Println(fmt.Sprintf("webusb - connect - current configuration err %s", err.Error()))
	} else {
		b.mw.Println(fmt.Sprintf("webusb - connect - current configuration %d", currConf))
	}

	b.mw.Println("webusb - connect - claiming interface")
	err = lowlevel.Claim_Interface(d, webIfaceNum)
	if err != nil {
		b.mw.Println("webusb - connect - claiming interface failed")
		lowlevel.Close(d)
		return nil, err
	}

	b.mw.Println("webusb - connect - claiming interface done")

	return &WUD{
		dev:    d,
		closed: 0,

		mw:     b.mw,
		cancel: b.cancel,
	}, nil
}

func matchType(dd *lowlevel.Device_Descriptor) core.DeviceType {
	if dd.IdProduct == core.ProductT1Firmware {
		// this is HID, in platforms where we don't use hidapi (linux, bsd)
		return core.TypeT1Hid
	}

	if dd.IdProduct == core.ProductT2Bootloader {
		if int(dd.BcdDevice>>8) == 1 {
			return core.TypeT1WebusbBoot
		}
		return core.TypeT2Boot
	}

	if int(dd.BcdDevice>>8) == 1 {
		return core.TypeT1Webusb
	}

	return core.TypeT2
}

func (b *WebUSB) match(dev lowlevel.Device) (bool, core.DeviceType) {
	dd, err := lowlevel.Get_Device_Descriptor(dev)
	if err != nil {
		b.mw.Println("webusb - match - error getting descriptor -" + err.Error())
		return false, 0
	}

	vid := dd.IdVendor
	pid := dd.IdProduct
	if !b.matchVidPid(vid, pid) {
		return false, 0
	}

	c, err := lowlevel.Get_Active_Config_Descriptor(dev)
	if err != nil {
		b.mw.Println("webusb - match - error getting config descriptor " + err.Error())
		return false, 0
	}

	var is bool
	if b.only {

		// if we don't use hidapi at all, keep HID devices
		is = (c.BNumInterfaces > webIfaceNum &&
			c.Interface[webIfaceNum].Num_altsetting > webAltSetting)

	} else {

		is = (c.BNumInterfaces > webIfaceNum &&
			c.Interface[webIfaceNum].Num_altsetting > webAltSetting &&
			c.Interface[webIfaceNum].Altsetting[webAltSetting].BInterfaceClass == lowlevel.CLASS_VENDOR_SPEC)
	}

	if !is {
		return false, 0
	}
	return true, matchType(dd)

}

func (b *WebUSB) matchVidPid(vid uint16, pid uint16) bool {
	// Note: Trezor1 webusb will actually have the T2 vid/pid
	trezor2 := vid == core.VendorT2 && (pid == core.ProductT2Firmware || pid == core.ProductT2Bootloader)

	if b.only {
		trezor1 := vid == core.VendorT1 && (pid == core.ProductT1Firmware)
		return trezor1 || trezor2
	}

	return trezor2
}

func (b *WebUSB) identify(dev lowlevel.Device) string {
	var ports [8]byte
	p, err := lowlevel.Get_Port_Numbers(dev, ports[:])
	if err != nil {
		b.mw.Println(fmt.Sprintf("webusb - identify - error getting port numbers %s", err.Error()))
		return ""
	}
	return webusbPrefix + hex.EncodeToString(p)
}

type WUD struct {
	dev lowlevel.Device_Handle

	closed        int32 // atomic
	transferMutex sync.Mutex
	// two interrupt_transfers should not happen at the same time

	cancel bool

	mw *memorywriter.MemoryWriter
}

func (d *WUD) Close(disconnected bool) error {
	d.mw.Println("webusb - close - storing d.closed")
	atomic.StoreInt32(&d.closed, 1)

	if d.cancel {
		// libusb close does NOT cancel transfers on close
		// => we are using our own function that we added to libusb/sync.c
		// this "unblocks" Interrupt_Transfer in readWrite

		d.mw.Println("webusb - close - canceling previous transfers")
		lowlevel.Cancel_Sync_Transfers_On_Device(d.dev)

		// reading recently disconnected device sometimes causes weird issues
		// => if we *know* it is disconnected, don't finish read queue
		//
		// Finishing read queue is not necessary when we don't allow cancelling
		// (since when we don't allow cancelling, we don't allow session stealing)
		if !disconnected {
			d.mw.Println("webusb - close - finishing read queue")
			d.finishReadQueue()
		}
	}

	d.mw.Println("webusb - close - low level close")
	lowlevel.Close(d.dev)
	d.mw.Println("webusb - close - done")

	return nil
}

func (d *WUD) finishReadQueue() {
	d.mw.Println("webusb - close - rq - wait for transfermutex lock")
	d.transferMutex.Lock()
	var err error
	var buf [64]byte

	for err == nil {
		// these transfers have timeouts => should not interfer with
		// cancel_sync_transfers_on_device
		d.mw.Println("webusb - close - rq - transfer")
		_, err = lowlevel.Interrupt_Transfer(d.dev, webEpIn, buf[:], 50)
	}
	d.transferMutex.Unlock()
	d.mw.Println("webusb - close - rq - done")
}

func (d *WUD) readWrite(buf []byte, endpoint uint8) (int, error) {
	d.mw.Println("webusb - rw - start")
	for {
		d.mw.Println("webusb - rw - checking closed")
		closed := (atomic.LoadInt32(&d.closed)) == 1
		if closed {
			d.mw.Println("webusb - rw - closed, skip")
			return 0, errClosedDevice
		}

		d.mw.Println("webusb - rw - lock transfer mutex")
		d.transferMutex.Lock()
		d.mw.Println("webusb - rw - actual interrupt transport")
		// This has no timeout, but is stopped by Cancel_Sync_Transfers_On_Device
		p, err := lowlevel.Interrupt_Transfer(d.dev, endpoint, buf, 0)
		d.transferMutex.Unlock()
		d.mw.Println("webusb - rw - single transfer done")

		if err != nil {
			d.mw.Println(fmt.Sprintf("webusb - rw - error seen - %s", err.Error()))
			if isErrorDisconnect(err) {
				d.mw.Println("webusb - rw - device probably disconnected")
				return 0, errDisconnect
			}

			d.mw.Println("webusb - rw - other error")
			return 0, err
		}

		// sometimes, empty report is read, skip it
		// TODO: is this still needed with 0 timeouts?
		if len(p) > 0 {
			d.mw.Println("webusb - rw - single transfer succesful")
			return len(p), err
		}
		d.mw.Println("webusb - rw - skipping empty transfer, go again")
		// continue the for cycle if empty transfer
	}
}

func isErrorDisconnect(err error) bool {
	// according to libusb docs, disconnecting device should cause only
	// LIBUSB_ERROR_NO_DEVICE error, but in real life, it causes also
	// LIBUSB_ERROR_IO, LIBUSB_ERROR_PIPE, LIBUSB_ERROR_OTHER

	return (err.Error() == lowlevel.Error_Name(int(lowlevel.ERROR_IO)) ||
		err.Error() == lowlevel.Error_Name(int(lowlevel.ERROR_NO_DEVICE)) ||
		err.Error() == lowlevel.Error_Name(int(lowlevel.ERROR_OTHER)) ||
		err.Error() == lowlevel.Error_Name(int(lowlevel.ERROR_PIPE)))
}

func (d *WUD) Write(buf []byte) (int, error) {
	d.mw.Println("webusb - rw - write start")
	return d.readWrite(buf, webEpOut)
}

func (d *WUD) Read(buf []byte) (int, error) {
	d.mw.Println("webusb - rw - read start")
	return d.readWrite(buf, webEpIn)
}
