/*
Package ssdplog provides log mechanism for ssdp.
*/
package ssdplog

import "log"

//var LoggerProvider = func() *log.Logger { return nil }
var LoggerProvider = func() *log.Logger { return log.Default() }

func Printf(s string, a ...any) {
	if p := LoggerProvider; p != nil {
		if l := p(); l != nil {
			l.Printf(s, a...)
		}
	}
}
