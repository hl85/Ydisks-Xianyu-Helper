package mtop

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestDetectItemMultiSpec 封装TestDetect商品MultiSpec业务协调。
func TestDetectItemMultiSpec(t *testing.T) {
	// server 用于本次流程后续判断的server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") != "mtop.taobao.idle.pc.detail" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		// rawBody 用于本次流程后续判断的原始请求体
		rawBody, _ := io.ReadAll(r.Body)
		// form 用于本次流程后续判断的表单
		form, _ := url.ParseQuery(string(rawBody))
		if form.Get("data") != `{"itemId":"item-1"}` {
			t.Errorf("data=%q", form.Get("data"))
		}
		_, _ = io.WriteString(w, `{"ret":["SUCCESS::调用成功"],"data":{"skuDO":{"skuProperties":[{"name":"颜色"}],"skuList":[{"id":"1"},{"id":"2"}]}}}`)
	}))
	defer server.Close()
	// client 用于本次流程后续判断的client
	client := &ClientImpl{HTTPClient: server.Client(), ItemDetailURL: server.URL}
	// got、err 用于本次流程后续判断的got、err
	got, err := client.DetectItemMultiSpec(context.Background(), "_m_h5_tk=token_1", "item-1")
	if err != nil || !got {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

// TestDetectItemMultiSpecSignals 封装TestDetect商品MultiSpecSignals业务协调。
func TestDetectItemMultiSpecSignals(t *testing.T) {
	// cases 用于本次流程后续判断的cases
	cases := []struct {
		name string
		data any
		want bool
	}{
		{name: "explicit", data: map[string]any{"multiSKU": true}, want: true},
		{name: "two skus", data: map[string]any{"skuList": []any{1, 2}}, want: true},
		{name: "sku props", data: map[string]any{"skuDO": map[string]any{"props": []any{"颜色"}}}, want: true},
		{name: "single", data: map[string]any{"multiSKU": false, "skuList": []any{1}}, want: false},
	}
	// tc 表示当前遍历过程中的tc
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if // got 用于本次流程后续判断的got
			got := detectItemMultiSpec(tc.data); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// TestDetectItemMultiSpecRejectsMissingToken 封装TestDetect商品MultiSpecRejectsMissing令牌业务协调。
// 缺少签名令牌会先尝试向令牌接口补齐；补齐失败时必须仍然报出缺令牌原因。
func TestDetectItemMultiSpecRejectsMissingToken(t *testing.T) {
	// tokenServer 模拟令牌接口：返回不可重试的失败，使补齐立刻失败，测试保持快速。
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ret":["FAIL_SYS_ILLEGAL_ACCESS::非法请求"]}`)
	}))
	defer tokenServer.Close()
	// client 用于本次流程后续判断的client
	client := &ClientImpl{HTTPClient: tokenServer.Client(), TokenURL: tokenServer.URL}
	if // err 用于本次流程后续判断的err
	_, err := client.DetectItemMultiSpec(context.Background(), "unb=1", "item-1"); err == nil || !strings.Contains(err.Error(), "_m_h5_tk") {
		t.Fatalf("err=%v", err)
	}
}
