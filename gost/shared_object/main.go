package main

/*
#include <stdlib.h>
*/
import "C"
import (
	"os"
	"unsafe"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/loader"
	"github.com/go-gost/x/config/parsing/parser"
	"github.com/go-gost/x/registry"
)

func main() {

}

const basepath = "/data/user/0/one.sian.gost"
const configFile = basepath + "/config.yaml"

func init() {
	fixTimezone()
	fixDNSResolver()
}

//export Version
func Version() *C.char {
	v := C.CString(version)
	return v
}

//export FreeCString
func FreeCString(value *C.char) {
	C.free(unsafe.Pointer(value))
}

//export Start
func Start(cfg *C.char) *C.char {
	cfgContent := C.GoString(cfg)
	if cfgContent == "" {
		return C.CString("success")
	}
	if err := os.WriteFile(configFile, []byte(cfgContent), 0666); err != nil {
		return C.CString(err.Error())
	}
	parser.Init(parser.Args{
		CfgFile:     configFile,
		Services:    nil,
		Nodes:       nil,
		Debug:       false,
		Trace:       false,
		ApiAddr:     "",
		MetricsAddr: "",
	})

	cfgData, err := parser.Parse()
	if err != nil {
		return C.CString(err.Error())
	}

	config.Set(cfgData)

	if err := loader.Load(cfgData); err != nil {
		return C.CString(err.Error())
	}

	if err := run(); err != nil {
		return C.CString(err.Error())
	}

	return C.CString("success")
}

//export Stop
func Stop() {
	for name, srv := range registry.ServiceRegistry().GetAll() {
		_ = srv.Close()
		logger.Default().Debugf("service %s shutdown", name)
	}
}

//export ReloadConfig
func ReloadConfig(cfg *C.char) *C.char {
	cfgContent := C.GoString(cfg)
	if cfgContent == "" {
		return C.CString("success")
	}
	if err := os.WriteFile(configFile, []byte(cfgContent), 0666); err != nil {
		return C.CString(err.Error())
	}

	cfgData, err := parser.Parse()
	if err != nil {
		return C.CString(err.Error())
	}
	config.Set(cfgData)

	if err := loader.Load(cfgData); err != nil {
		return C.CString(err.Error())
	}

	if err := run(); err != nil {
		return C.CString(err.Error())
	}

	return C.CString("success")
}

func run() error {
	for _, svc := range registry.ServiceRegistry().GetAll() {
		s := svc
		go s.Serve()
	}
	return nil
}
