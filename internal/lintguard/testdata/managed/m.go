package managed

type Finding struct{ Kind string }

const (
	FindingDumpStalled = "DUMP_STALLED"
	FindingOther       = "OTHER"
)

type sink struct{}

func (sink) Report(Finding)                       {}
func (sink) Resolve(string, string, string)       {}
func (sink) SignalSuccess(string, string, string) {}

var s sink
var tasks = struct{ FindingDumpStalled string }{FindingDumpStalled}

type logger struct{}

func (logger) Report(...any) {}

func methodReport()                  { s.Report(Finding{Kind: FindingDumpStalled}) }
func selectorConst()                 { s.Resolve("1", tasks.FindingDumpStalled, "") }
func loggerMethod()                  { logger{}.Report("stalled", FindingDumpStalled) }
func plainFunc(report func(Finding)) { report(Finding{Kind: FindingDumpStalled}) }
func resolveLiteral()                { s.Resolve("1", "DUMP_STALLED", "") }
func signalOK()                      { s.SignalSuccess("1", FindingDumpStalled, "") }
func unmanaged()                     { s.Report(Finding{Kind: FindingOther}) }
