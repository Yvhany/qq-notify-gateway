package main

import "testing"

func TestExtractOpenID(t *testing.T) {
	// 真实帧结构：op/t/d 包裹
	c2cFrame := []byte(`{"op":0,"s":2,"t":"C2C_MESSAGE_CREATE","d":{"author":{"bot":false,"user_openid":"OID_C2C_123"},"content":"hello","message_type":0}}`)
	if got := extractOpenID("c2c", c2cFrame); got != "OID_C2C_123" {
		t.Errorf("c2c openid 错误: %q", got)
	}

	groupFrame := []byte(`{"op":0,"s":3,"t":"GROUP_AT_MESSAGE_CREATE","d":{"group_openid":"OID_GROUP_9","author":{"user_openid":"X"},"content":"@bot hi"}}`)
	if got := extractOpenID("group", groupFrame); got != "OID_GROUP_9" {
		t.Errorf("group openid 错误: %q", got)
	}

	if got := extractOpenID("c2c", []byte(`{"d":{"author":{}}}`)); got != "" {
		t.Errorf("缺失字段应返回空，得到 %q", got)
	}
	if got := extractOpenID("c2c", []byte(`not-json`)); got != "" {
		t.Errorf("坏 JSON 应返回空，得到 %q", got)
	}
	if got := extractOpenID("group", []byte(`{"op":0}`)); got != "" {
		t.Errorf("缺 d 字段应返回空，得到 %q", got)
	}
}
