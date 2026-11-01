package oncall

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

const anaPhone = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

func TestPhonesRegisterAndPagesReachThem(t *testing.T) {
	hs := newHarnessFrom(t, testConfig(filepath.Join(t.TempDir(), "oc.db"))+"push: {provider: oncall-push}\n", monday10)
	reg := func(token, id, user string) (int, map[string]any) {
		return hs.call("PUT", "/api/v1/devices/"+id, user+"-token", "",
			map[string]string{"platform": "ios", "token": token, "environment": "development", "name": "Suzuka"})
	}
	if status, body := reg(strings.ToUpper(anaPhone), "PHONE-1", "ana"); status != 200 || body["push"] != true {
		t.Fatalf("register: %d %v", status, body)
	}
	for name, body := range map[string]map[string]string{
		"not hex":        {"platform": "ios", "token": "zz", "environment": "development"},
		"android":        {"platform": "android", "token": anaPhone, "environment": "development"},
		"no environment": {"platform": "ios", "token": anaPhone},
	} {
		if status, _ := hs.call("PUT", "/api/v1/devices/PHONE-2", "ana-token", "", body); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}
	if status, _ := hs.call("PUT", "/api/v1/devices/bad%20id", "ana-token", "", map[string]string{}); status != http.StatusBadRequest {
		t.Errorf("bad id: %d", status)
	}
	if status, body := hs.call("GET", "/api/v1/devices", "ana-token", "", nil); status != 200 || len(body["devices"].([]any)) != 1 {
		t.Fatalf("list: %d %v", status, body)
	} else if raw, _ := json.Marshal(body); strings.Contains(string(raw), anaPhone) {
		t.Error("the token was shown back")
	}

	// A page goes to Ana's channel and to her phone, through the push provider.
	hs.send("jellyfin", firing("fp1", "Jellyfin streams failing"), 1)
	if len(hs.pages) != 2 || hs.pages[0].providers[0] != "telegram-ana" || hs.pages[1].providers[0] != "oncall-push" {
		t.Fatalf("pages %+v", hs.pages)
	}
	push := hs.pages[1].ev
	if push.Annotations["apns_tokens"] != "development:"+anaPhone || push.Annotations["apns_category"] != "INCIDENT_TRIGGERED" ||
		push.Annotations["apns_thread_id"] != "inc_1" || push.Annotations["apns_interruption_level"] != "time-sensitive" {
		t.Fatalf("push annotations %v", push.Annotations)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(push.Annotations["apns_data"]), &data); err != nil || data["incident_id"] != "inc_1" || data["status"] != "triggered" {
		t.Fatalf("apns_data %v (%v)", data, err)
	}
	if push.EventID == hs.pages[0].ev.EventID || hs.pages[0].ev.Annotations["apns_tokens"] != "" {
		t.Error("the push must be its own event, and the channel page must not carry tokens")
	}
	_, body := hs.call("GET", "/api/v1/incidents/inc_1/timeline", "ana-token", "", nil)
	if raw, _ := json.Marshal(body); !strings.Contains(string(raw), "Sent through telegram-ana and the Omni On-Call app") {
		t.Errorf("timeline: %s", raw)
	}

	// The same token on Ben's account (the phone changed hands) leaves Ana's list.
	if status, _ := reg(anaPhone, "PHONE-9", "ben"); status != 200 {
		t.Fatal("ben's registration failed")
	}
	if _, body := hs.call("GET", "/api/v1/devices", "ana-token", "", nil); len(body["devices"].([]any)) != 0 {
		t.Errorf("ana still has the moved phone: %v", body)
	}
	if status, _ := hs.call("DELETE", "/api/v1/devices/PHONE-9", "ana-token", "", nil); status != http.StatusNotFound {
		t.Errorf("ana deleted ben's phone: %d", status)
	}
	if status, _ := hs.call("DELETE", "/api/v1/devices/PHONE-9", "ben-token", "", nil); status != 200 {
		t.Errorf("ben couldn't delete his phone: %d", status)
	}
}

func TestPhonesAreNotPushedWithoutAPushProvider(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	if status, body := hs.call("PUT", "/api/v1/devices/P1", "ana-token", "",
		map[string]string{"platform": "ios", "token": anaPhone, "environment": "production"}); status != 200 || body["push"] != false {
		t.Fatalf("register: %d %v", status, body)
	}
	hs.send("jellyfin", firing("fp1", "Jellyfin streams failing"), 1)
	if len(hs.pages) != 1 || hs.pages[0].providers[0] != "telegram-ana" {
		t.Fatalf("pages %+v", hs.pages)
	}
}

func TestAPhoneAloneIsAContactMethod(t *testing.T) {
	cfg := strings.Replace(testConfig(filepath.Join(t.TempDir(), "oc.db")), ", contacts: [telegram-ana]", "", 1)
	hs := newHarnessFrom(t, cfg+"push: {provider: oncall-push}\n", monday10)
	hs.send("jellyfin", firing("fp0", "Before the phone"), 1)
	if len(hs.pages) != 0 {
		t.Fatalf("paged with no contact: %+v", hs.pages)
	}
	hs.call("PUT", "/api/v1/devices/P1", "ana-token", "", map[string]string{"platform": "ios", "token": anaPhone, "environment": "development"})
	hs.send("jellyfin", firing("fp1", "Jellyfin streams failing"), 2)
	if len(hs.pages) != 1 || hs.pages[0].providers[0] != "oncall-push" {
		t.Fatalf("pages %+v", hs.pages)
	}
}
