package plugin

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"runtime"
	"time"
	"unsafe"

	"github.com/calyptia/plugin/input"
	"github.com/calyptia/plugin/output"
	"github.com/ugorji/go/codec"

	cmetrics "github.com/calyptia/cmetrics-go"
)

var unregister func()

var counters = struct {
	Succedded *cmetrics.Counter
	Failed    *cmetrics.Counter
}{}

func setupMetrics(cmt *cmetrics.Context, name string) error {
	var err error
	counters.Succedded, err = cmt.CounterCreate("fluentbit", "input",
		"operation_succeeded_total", "Total number of succeeded operations",
		[]string{name},
	)
	if err != nil {
		return err
	}

	counters.Failed, err = cmt.CounterCreate("fluentbit", "input",
		"operation_failed_total", "Total number of failed operations",
		[]string{name},
	)

	return err
}

// FLBPluginRegister registers a plugin in the context of the fluent-bit runtime, a name and description
// can be provided.
//export FLBPluginRegister
func FLBPluginRegister(def unsafe.Pointer) int {
	defer registerWG.Done()

	if theInput == nil && theOutput == nil {
		fmt.Fprintf(os.Stderr, "no input or output registered\n")
		return input.FLB_RETRY
	}

	if theInput != nil {
		out := input.FLBPluginRegister(def, theName, theDesc)
		unregister = func() {
			input.FLBPluginUnregister(def)
		}
		return out
	}

	out := output.FLBPluginRegister(def, theName, theDesc)
	unregister = func() {
		output.FLBPluginUnregister(def)
	}

	return out
}

// FLBPluginInit this method gets invoked once by the fluent-bit runtime at initialisation phase.
// here all the plugin context should be initialised and any data or flag required for
// plugins to execute the collect or flush callback.
//export FLBPluginInit
func FLBPluginInit(ptr unsafe.Pointer) int {
	defer initWG.Done()

	registerWG.Wait()

	if theInput == nil && theOutput == nil {
		fmt.Fprintf(os.Stderr, "no input or output registered\n")
		return input.FLB_RETRY
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var err error
	if theInput != nil {
		conf := &flbInputConfigLoader{ptr: ptr}
		cmt, err := input.FLBPluginGetCMetricsContext(ptr)
		if err != nil {
			return input.FLB_ERROR
		}
		err = setupMetrics(cmt, theName)
		if err == nil {
			err = theInput.Init(ctx, conf)
		}
	} else {
		conf := &flbOutputConfigLoader{ptr: ptr}
		cmt, err := output.FLBPluginGetCMetricsContext(ptr)
		if err != nil {
			return output.FLB_ERROR
		}
		err = setupMetrics(cmt, theName)
		if err == nil {
			err = theOutput.Init(ctx, conf)
		}
	}
	if err != nil {
		_ = counters.Failed.Inc(time.Now(), []string{theName})
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		return input.FLB_ERROR
	}

	_ = counters.Succedded.Inc(time.Now(), []string{theName})
	return input.FLB_OK
}

// FLBPluginInputCallback this method gets invoked by the fluent-bit runtime, once the plugin has been
// initialised, the plugin implementation is responsible for handling the incoming data and the context
// that gets past, for long-living collectors the plugin itself should keep a running thread and fluent-bit
// will not execute further callbacks.
//export FLBPluginInputCallback
func FLBPluginInputCallback(data *unsafe.Pointer, csize *C.size_t) int {
	initWG.Wait()

	if theInput == nil {
		fmt.Fprintf(os.Stderr, "no input registered\n")
		return input.FLB_RETRY
	}

	var err error
	once.Do(func() {
		runCtx, runCancel = context.WithCancel(context.Background())
		theChannel = make(chan Message, 1)
		go func() {
			err = theInput.Collect(runCtx, theChannel)
		}()
	})
	if err != nil {
		_ = counters.Failed.Inc(time.Now(), []string{theName})
		fmt.Fprintf(os.Stderr, "run: %s\n", err)
		return input.FLB_ERROR
	}

	select {
	case msg, ok := <-theChannel:
		if !ok {
			_ = counters.Succedded.Inc(time.Now(), []string{theName})
			return input.FLB_OK
		}

		t := input.FLBTime{Time: msg.Time}
		b, err := input.NewEncoder().Encode([]any{t, msg.Record})
		if err != nil {
			_ = counters.Failed.Inc(time.Now(), []string{theName})
			fmt.Fprintf(os.Stderr, "encode: %s\n", err)
			return input.FLB_ERROR
		}

		cdata := C.CBytes(b)

		*data = cdata
		*csize = C.size_t(len(b))

		// C.free(unsafe.Pointer(cdata))
	case <-runCtx.Done():
		err := runCtx.Err()
		if err != nil && !errors.Is(err, context.Canceled) {
			_ = counters.Failed.Inc(time.Now(), []string{theName})
			fmt.Fprintf(os.Stderr, "run: %s\n", err)
			return input.FLB_ERROR
		}
		// enforce a runtime gc, to prevent the thread finalizer on
		// fluent-bit to kick in before any remaining data has not been GC'ed
		// causing a sigsegv.
		defer runtime.GC()
	}

	_ = counters.Succedded.Inc(time.Now(), []string{theName})
	return input.FLB_OK
}

// FLBPluginFlush callback gets invoked by the fluent-bit runtime once there is data for the corresponding
// plugin in the pipeline, a data pointer, length and a tag are passed to the plugin interface implementation.
//export FLBPluginFlush
//nolint:funlen,gocognit,gocyclo //ignore length requirement for this function, TODO: refactor into smaller functions.
func FLBPluginFlush(data unsafe.Pointer, clength C.int, ctag *C.char) int {
	initWG.Wait()

	if theOutput == nil {
		fmt.Fprintf(os.Stderr, "no output registered\n")
		return output.FLB_RETRY
	}

	var err error
	once.Do(func() {
		runCtx, runCancel = context.WithCancel(context.Background())
		theChannel = make(chan Message, 1)
		go func() {
			err = theOutput.Flush(runCtx, theChannel)
		}()
	})
	if err != nil {
		_ = counters.Failed.Inc(time.Now(), []string{theName})
		fmt.Fprintf(os.Stderr, "run: %s\n", err)
		return output.FLB_ERROR
	}

	select {
	case <-runCtx.Done():
		err = runCtx.Err()
		if err != nil && !errors.Is(err, context.Canceled) {
			_ = counters.Failed.Inc(time.Now(), []string{theName})
			fmt.Fprintf(os.Stderr, "run: %s\n", err)
			return output.FLB_ERROR
		}

		_ = counters.Succedded.Inc(time.Now(), []string{theName})
		return output.FLB_OK
	default:
	}

	in := C.GoBytes(data, clength)
	h := &codec.MsgpackHandle{}
	err = h.SetBytesExt(reflect.TypeOf(bigEndianTime{}), 0, &bigEndianTime{})
	if err != nil {
		_ = counters.Failed.Inc(time.Now(), []string{theName})
		fmt.Fprintf(os.Stderr, "big endian time bytes ext: %v\n", err)
		return output.FLB_ERROR
	}

	dec := codec.NewDecoderBytes(in, h)

	for {
		select {
		case <-runCtx.Done():
			err := runCtx.Err()
			if err != nil && !errors.Is(err, context.Canceled) {
				_ = counters.Failed.Inc(time.Now(), []string{theName})
				fmt.Fprintf(os.Stderr, "run: %s\n", err)
				return output.FLB_ERROR
			}

			_ = counters.Succedded.Inc(time.Now(), []string{theName})
			return output.FLB_OK
		default:
		}

		var entry []any
		err := dec.Decode(&entry)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			_ = counters.Failed.Inc(time.Now(), []string{theName})
			fmt.Fprintf(os.Stderr, "decode: %s\n", err)
			return output.FLB_ERROR
		}

		if d := len(entry); d != 2 {
			_ = counters.Failed.Inc(time.Now(), []string{theName})
			fmt.Fprintf(os.Stderr, "unexpected entry length: %d\n", d)
			return output.FLB_ERROR
		}

		ft, ok := entry[0].(bigEndianTime)
		if !ok {
			_ = counters.Failed.Inc(time.Now(), []string{theName})
			fmt.Fprintf(os.Stderr, "unexpected entry time type: %T\n", entry[0])
			return output.FLB_ERROR
		}

		t := time.Time(ft)

		recVal, ok := entry[1].(map[any]any)
		if !ok {
			_ = counters.Failed.Inc(time.Now(), []string{theName})
			fmt.Fprintf(os.Stderr, "unexpected entry record type: %T\n", entry[1])
			return output.FLB_ERROR
		}

		var rec map[string]string
		if d := len(recVal); d != 0 {
			rec = make(map[string]string, d)
			for k, v := range recVal {
				key, ok := k.(string)
				if !ok {
					_ = counters.Failed.Inc(time.Now(), []string{theName})
					fmt.Fprintf(os.Stderr, "unexpected record key type: %T\n", k)
					return output.FLB_ERROR
				}

				val, ok := v.([]uint8)
				if !ok {
					_ = counters.Failed.Inc(time.Now(), []string{theName})
					fmt.Fprintf(os.Stderr, "unexpected record value type: %T\n", v)
					return output.FLB_ERROR
				}

				rec[key] = string(val)
			}
		}

		tag := C.GoString(ctag)
		// C.free(unsafe.Pointer(ctag))

		theChannel <- Message{Time: t, Record: rec, tag: &tag}

		// C.free(data)
		// C.free(unsafe.Pointer(&clength))
	}

	_ = counters.Succedded.Inc(time.Now(), []string{theName})
	return output.FLB_OK
}

// FLBPluginExit method is invoked once the plugin instance is exited from the fluent-bit context.
//export FLBPluginExit
func FLBPluginExit() int {
	log.Printf("calling FLBPluginExit(): name=%q\n", theName)

	if unregister != nil {
		unregister()
	}

	if runCancel != nil {
		runCancel()
	}

	if theChannel != nil {
		defer close(theChannel)
	}

	_ = counters.Succedded.Inc(time.Now(), []string{theName})
	return input.FLB_OK
}

type flbInputConfigLoader struct {
	ptr unsafe.Pointer
}

func (f *flbInputConfigLoader) String(key string) string {
	return input.FLBPluginConfigKey(f.ptr, key)
}

type flbOutputConfigLoader struct {
	ptr unsafe.Pointer
}

func (f *flbOutputConfigLoader) String(key string) string {
	return output.FLBPluginConfigKey(f.ptr, key)
}
