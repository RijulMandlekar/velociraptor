// +build windows

// These VQL plugins deal with Windows WMI.
package wmi

// #cgo LDFLAGS: -lole32 -lwbemuuid -loleaut32 -luuid
//
// void *watchEvents(void *go_ctx, char *query, char *namespace);
//
// void destroyEvent(void *c_ctx);
import "C"

import (
	"context"
	"runtime"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	pointer "github.com/mattn/go-pointer"
	vql_subsystem "www.velocidex.com/golang/velociraptor/vql"
	wmi_parse "www.velocidex.com/golang/velociraptor/vql/windows/wmi/parse"
	vfilter "www.velocidex.com/golang/vfilter"
)

type WMIObject struct {
	Raw    string
	parsed *vfilter.Dict
}

func (self *WMIObject) Parse() (*vfilter.Dict, error) {
	if self.parsed != nil {
		return self.parsed, nil
	}

	mof, err := wmi_parse.Parse(self.Raw)
	if err != nil {
		return nil, err
	}
	self.parsed = mof.ToDict()
	return self.parsed, nil
}

type eventQueryContext struct {
	output chan vfilter.Row
	scope  *vfilter.Scope
}

// This is called to handle the serialized event string. We just send
// it down the channel.
func (self *eventQueryContext) ProcessEvent(event string) {
	select {
	case self.output <- &WMIObject{Raw: event}:
	default:
		// We can not send the message because the queue is
		// too full. We have no choice but to drop it.
	}
}

func (self *eventQueryContext) Log(message string) {
	self.scope.Log(message)
}

//export process_event
func process_event(ctx *C.int, bstring **C.ushort) {
	go_ctx := pointer.Restore(unsafe.Pointer(ctx)).(*eventQueryContext)
	text := ole.BstrToString(*(**uint16)(unsafe.Pointer(bstring)))
	go_ctx.ProcessEvent(text)
}

//export log_error
func log_error(ctx *C.int, message *C.char) {
	go_ctx := pointer.Restore(unsafe.Pointer(ctx)).(*eventQueryContext)
	go_ctx.Log(C.GoString(message))
}

type WmiEventPluginArgs struct {
	Query     string `vfilter:"required,field=query"`
	Namespace string `vfilter:"required,field=namespace"`

	// How long to wait for events.
	Wait int64 `vfilter:"required,field=wait"`
}

type WmiEventPlugin struct{}

func (self WmiEventPlugin) Call(
	ctx context.Context,
	scope *vfilter.Scope,
	args *vfilter.Dict) <-chan vfilter.Row {
	output_chan := make(chan vfilter.Row)
	arg := &WmiEventPluginArgs{}

	go func() {
		defer close(output_chan)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		err := vfilter.ExtractArgs(scope, args, arg)
		if err != nil {
			scope.Log("wmi_events: %s", err.Error())
			return
		}

		if arg.Namespace == "" {
			arg.Namespace = "ROOT/CIMV2"
		}

		sub_ctx, cancel := context.WithTimeout(
			ctx, time.Duration(arg.Wait)*time.Second)
		defer cancel()

		event_context := eventQueryContext{
			// Queue up to 100 messages
			output: make(chan vfilter.Row, 100),
			scope:  scope,
		}
		defer close(event_context.output)

		ptr := pointer.Save(&event_context)
		defer pointer.Unref(ptr)

		c_ctx := C.watchEvents(ptr, C.CString(arg.Query),
			C.CString(arg.Namespace))
		if c_ctx == nil {
			return
		}

		for {
			select {
			case <-sub_ctx.Done():
				// Destroy the C context - we are done here.
				C.destroyEvent(c_ctx)
				return

				// Read the next item from the event
				// queue and send it to the VQL
				// subsystem.
			case item, ok := <-event_context.output:
				if !ok {
					return
				}
				output_chan <- item
			}
		}
	}()

	return output_chan
}

func (self WmiEventPlugin) Info(type_map *vfilter.TypeMap) *vfilter.PluginInfo {
	return &vfilter.PluginInfo{
		Name:    "wmi_events",
		Doc:     "Executes an evented WMI queries asynchronously.",
		ArgType: type_map.AddType(&WmiEventPluginArgs{}),
	}
}

func init() {
	vql_subsystem.RegisterPlugin(&WmiEventPlugin{})
}