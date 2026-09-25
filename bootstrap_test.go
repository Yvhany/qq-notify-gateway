package main

import "testing"

func TestExtractOpenID(t *testing.T) {
	c2c := []byte(`{"author":{"id":"1","user_openid":"OID_C2C_123"},"content":"hello","msg_id":"m1"}`)
	if got := extractOpenID("c2c", c2c); got != "OID_C2C_123" {
		t.Errorf("c2c openid 错误: %q", got)
	}

	group := []byte(`{"group_openid":"OID_GROUP_9","author":{"user_openid":"X"},"content":"@bot hi"}`)
	if got := extractOpenID("group", group); got != "OID_GROUP_9" {
		t.Errorf("group openid 错误: %q", got)
	}

	if got := extractOpenID("c2c", []byte(`{"author":{}}`)); got != "" {
		t.Errorf("缺失字段应返回空，得到 %q", got)
	}
	if got := extractOpenID("c2c", []byte(`not-json`)); got != "" {
		t.Errorf("坏 JSON 应返回空，得到 %q", got)
	}
	if got := extractOpenID("group", []byte(`{}`)); got != "" {
		t.Errorf("空对象应返回空，得到 %q", got)
	}
}
