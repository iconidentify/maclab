package agent

import "testing"

func TestParseSleepSignal(t *testing.T) {
	for _, c := range []struct {
		line         string
		sleeping, ok bool
	}{
		{`{"type":"signal","sender":":1.3","path":"/org/freedesktop/login1","interface":"org.freedesktop.login1.Manager","member":"PrepareForSleep","payload":{"type":"b","data":[true]}}`, true, true},
		{`{"type":"signal","member":"PrepareForSleep","payload":{"type":"b","data":[false]}}`, false, true},
		{`{"type":"method_return","sender":"org.freedesktop.DBus","payload":{"type":"s","data":[":1.58"]}}`, false, false},
		{`{"type":"signal","member":"PrepareForShutdown","payload":{"type":"b","data":[true]}}`, false, false},
		{`not json`, false, false},
	} {
		if s, ok := parseSleepSignal([]byte(c.line)); s != c.sleeping || ok != c.ok {
			t.Errorf("%s: got %v %v", c.line, s, ok)
		}
	}
}
