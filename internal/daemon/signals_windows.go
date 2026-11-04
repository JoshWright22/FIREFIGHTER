package daemon

import "os"

var stopSignals = []os.Signal{os.Interrupt}
