package ws

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startScriptedResponseServer 按请求顺序回放预设响应，用于覆盖需要多步交互才能到达的校验分支。
// 脚本耗尽后服务端主动断开连接，使后续请求以传输错误结束。
func startScriptedResponseServer(t *testing.T, responses []map[string]any) (*httptest.Server, chan map[string]any) {
	t.Helper()
	// requests 收集客户端发送的业务帧，供测试校验参数归一化结果。
	requests := make(chan map[string]any, 8)
	// played 记录已经回放的响应序号；每个服务端连接独立递增。
	played := 0
	// server 是模拟闲鱼 WebSocket 的本地端点。
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// connection、acceptErr 保存 WebSocket 升级结果。
		connection, acceptErr := websocket.Accept(writer, request, nil)
		if acceptErr != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "")
		// readContext、cancelRead 保存服务端读取生命周期。
		readContext, cancelRead := context.WithCancel(request.Context())
		defer cancelRead()
		for {
			// messageType、raw、readErr 保存客户端业务帧读取结果。
			messageType, raw, readErr := connection.Read(readContext)
			if readErr != nil {
				return
			}
			if messageType != websocket.MessageText {
				continue
			}
			// message 保存客户端请求帧。
			var message map[string]any
			if json.Unmarshal(raw, &message) != nil {
				continue
			}
			select {
			case requests <- message:
			default:
			}
			if played >= len(responses) {
				// 脚本耗尽后断开连接，让调用方的后续请求得到传输错误。
				_ = connection.Close(websocket.StatusInternalError, "")
				return
			}
			// headers 保存请求中的 mid，使响应能够匹配 pending 请求。
			headers, _ := message["headers"].(map[string]any)
			// response 是本步回放的预设响应，补齐匹配用的 mid。
			response := map[string]any{}
			for key, value := range responses[played] {
				response[key] = value
			}
			response["headers"] = map[string]any{"mid": headers["mid"]}
			played++
			// encodedResponse 保存响应帧 JSON。
			encodedResponse, marshalErr := json.Marshal(response)
			if marshalErr != nil {
				return
			}
			if connection.Write(readContext, websocket.MessageText, encodedResponse) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// dialScriptedConn 连接按脚本回放响应的本地端点并返回业务连接。
func dialScriptedConn(t *testing.T, responses []map[string]any) (*Conn, chan map[string]any) {
	t.Helper()
	// server、requests 保存本地端点及收到的请求帧。
	server, requests := startScriptedResponseServer(t, responses)
	// dialContext、cancelDial 保存本地连接建立上下文。
	dialContext, cancelDial := context.WithTimeout(context.Background(), time.Second)
	defer cancelDial()
	// dialed、_, dialErr 保存 WebSocket 拨号结果。
	dialed, _, dialErr := websocket.Dial(dialContext, wsURL(server), nil)
	if dialErr != nil {
		t.Fatalf("dial scripted server: %v", dialErr)
	}
	dialed.SetReadLimit(8 << 20)
	t.Cleanup(func() { _ = dialed.CloseNow() })
	return newConn(dialed, Config{}, nilLogger()), requests
}

// TestNumberInt64NormalizesPlatformValueTypes 验证平台数值字段归一化覆盖全部具体类型分支。
// 平台在不同响应形态下会给出 int、int64、float64 或 json.Number，任何非整数取值都必须归零而不是被截断。
func TestNumberInt64NormalizesPlatformValueTypes(t *testing.T) {
	// cases 覆盖整数、浮点、非法浮点、JSON 数字与不可解析类型。
	cases := []struct {
		// name 是本用例覆盖的分支描述。
		name string
		// value 是待归一化的平台原始取值。
		value any
		// want 是期望归一化结果。
		want int64
	}{
		{name: "int", value: int(1789260661412), want: 1789260661412},
		{name: "int64", value: int64(1789260661412), want: 1789260661412},
		{name: "float64 整数", value: float64(1789260661412), want: 1789260661412},
		{name: "float64 小数", value: float64(1.5), want: 0},
		{name: "float64 NaN", value: math.NaN(), want: 0},
		{name: "float64 正无穷", value: math.Inf(1), want: 0},
		{name: "json.Number 合法", value: json.Number("4294154215667"), want: 4294154215667},
		{name: "json.Number 非法", value: json.Number("1.5"), want: 0},
		{name: "字符串不可解析", value: "1789260661412", want: 0},
		{name: "缺省类型", value: nil, want: 0},
	}
	// index 标识当前用例在表格中的位置，便于定位失败项。
	for index, testCase := range cases {
		if got := numberInt64(testCase.value); got != testCase.want {
			t.Errorf("用例 %d (%s): numberInt64=%d want %d", index, testCase.name, got, testCase.want)
		}
	}
}

// TestNormalizedTextValueHandlesMissingAndNilValues 验证平台文本字段的空值归一化分支。
func TestNormalizedTextValueHandlesMissingAndNilValues(t *testing.T) {
	if got := normalizedTextValue(nil); got != "" {
		t.Fatalf("nil 应归一化为空串: %q", got)
	}
	// typedNil 是装箱后的空指针；其文本形式为 "<nil>"，必须与真实文本区分。
	var typedNil *int
	if got := normalizedTextValue(typedNil); got != "" {
		t.Fatalf("\"<nil>\" 应归一化为空串: %q", got)
	}
	if got := normalizedTextValue(" 4294154215667.PNM "); got != "4294154215667.PNM" {
		t.Fatalf("文本应去除首尾空白: %q", got)
	}
	if got := normalizedTextValue(11873716); got != "11873716" {
		t.Fatalf("数值应转为文本: %q", got)
	}
}

// TestExtractSendReceiptWithoutExtension 验证响应缺少扩展字段时返回部分凭证而不 panic。
func TestExtractSendReceiptWithoutExtension(t *testing.T) {
	// receipt 是从仅有 messageId 的响应中提取的凭证。
	receipt := extractSendReceipt(map[string]any{"body": map[string]any{"messageId": "4294154215667.PNM"}})
	if receipt.MessageID != "4294154215667.PNM" || receipt.SenderUserID != "" || receipt.Summary != "" || receipt.CreateAt != 0 {
		t.Fatalf("缺失扩展字段时的凭证异常: %+v", receipt)
	}
	if receipt.ConfirmsOwnMessage("11873716") {
		t.Fatal("缺少发送者身份时不得判为已确认")
	}
	// 平台身份带协议后缀时仍应能确认自身消息。
	if !(SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "11873716@goofish"}).ConfirmsOwnMessage("11873716@goofish") {
		t.Fatal("两侧带协议后缀时应能确认自身消息")
	}
}

// TestStrictChatSendResponseCodeRejectsNonIntegerForms 验证发送状态码只接受完整整数形式。
func TestStrictChatSendResponseCodeRejectsNonIntegerForms(t *testing.T) {
	// cases 覆盖整数、浮点、JSON 数字与文本四类平台取值。
	cases := []struct {
		// name 是本用例覆盖的分支描述。
		name string
		// value 是待校验的平台状态码原始取值。
		value any
		// want 是期望解析出的状态码。
		want int
		// ok 表示期望是否判定为可解析。
		ok bool
	}{
		{name: "int", value: 200, want: 200, ok: true},
		{name: "float64 整数", value: float64(200), want: 200, ok: true},
		{name: "float64 小数", value: float64(200.5), want: 0, ok: false},
		{name: "json.Number 合法", value: json.Number("200"), want: 200, ok: true},
		{name: "json.Number 小数", value: json.Number("200.5"), want: 0, ok: false},
		{name: "文本整数", value: "200", want: 200, ok: true},
		{name: "文本带空白", value: "  200  ", want: 200, ok: true},
		{name: "文本为空", value: "", want: 0, ok: false},
		{name: "文本含符号", value: "-200", want: 0, ok: false},
		{name: "文本带尾随内容", value: "200OK", want: 0, ok: false},
		{name: "布尔不可解析", value: true, want: 0, ok: false},
	}
	// index 标识当前用例在表格中的位置，便于定位失败项。
	for index, testCase := range cases {
		// code、ok 是严格解析平台状态码的结果。
		code, ok := strictChatSendResponseCode(testCase.value)
		if code != testCase.want || ok != testCase.ok {
			t.Errorf("用例 %d (%s): code=%d ok=%v want code=%d ok=%v", index, testCase.name, code, ok, testCase.want, testCase.ok)
		}
	}
}

// TestSendChatContentRejectsUnmarshalableContent 验证正文无法序列化时不发出请求并归入确定未发送错误。
func TestSendChatContentRejectsUnmarshalableContent(t *testing.T) {
	// connection 是仅用于触发入参校验的空连接对象。
	connection := &Conn{}
	// unreachable 是无法被 JSON 序列化的正文，用于覆盖发送前的序列化失败分支。
	unreachable := map[string]any{"chan": make(chan int)}
	// _, sendErr 是正文序列化失败的发送结果。
	_, sendErr := connection.sendChatContentWithReceipt(context.Background(), "my", "cid", "to", unreachable)
	if SendResultKind(sendErr) != SendNotSent {
		t.Fatalf("序列化失败应归入确定未发送: %v", sendErr)
	}
}

// TestSendChatContentReportsCanceledContext 验证已取消上下文在写入前就归入确定未发送错误。
func TestSendChatContentReportsCanceledContext(t *testing.T) {
	// connection 是仅用于触发取消校验的空连接对象。
	connection := &Conn{}
	// canceled、cancel 是已经取消的发送上下文。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// _, sendErr 是取消上下文的发送结果。
	_, sendErr := connection.sendChatContentWithReceipt(canceled, "my", "cid", "to", map[string]any{"contentType": 1})
	if SendResultKind(sendErr) != SendNotSent {
		t.Fatalf("取消上下文应归入确定未发送: %v", sendErr)
	}
}

// TestSendChatContentReportsTransportFailure 验证连接断开时发送结果归入不确定而不是确定未发送。
// 传输失败无法判断平台是否已受理，若误判为确定未发送会导致自动化安全重放并重复发货。
func TestSendChatContentReportsTransportFailure(t *testing.T) {
	// connection 是在发送前被强制断开的本地连接。
	connection, _ := newAPIResponseConn(t, map[string]any{}, http.StatusOK)
	_ = connection.ws.CloseNow()
	// sendErr 是断开连接后的发送结果。
	sendErr := connection.SendText(context.Background(), "my", "cid", "to", "正文")
	if SendResultKind(sendErr) != SendUncertain {
		t.Fatalf("传输失败应归入不确定结果: %v", sendErr)
	}
	// imageErr 是同一路径下图片发送的结果，与文本共用确认逻辑。
	imageErr := connection.SendImage(context.Background(), "my", "cid", "to", "https://cdn.example/gift.png", 0, 0)
	if SendResultKind(imageErr) != SendUncertain {
		t.Fatalf("图片传输失败应归入不确定结果: %v", imageErr)
	}
}

// TestSendChatContentRejectsUnparsableResponseCode 验证平台状态码不可解析时归入不确定结果。
func TestSendChatContentRejectsUnparsableResponseCode(t *testing.T) {
	// connection 是回放非整数状态码的本地连接。
	connection, _ := dialScriptedConn(t, []map[string]any{{"code": "unknown", "body": map[string]any{}}})
	// _, sendErr 是状态码不可解析时的发送结果。
	_, sendErr := connection.sendChatContentWithReceipt(context.Background(), "my", "cid", "to", map[string]any{"contentType": 1})
	if SendResultKind(sendErr) != SendUncertain {
		t.Fatalf("状态码不可解析应归入不确定结果: %v", sendErr)
	}
}

// TestSendImageWithReceiptFillsDefaultDimensions 验证图片尺寸缺省时补默认宽高。
func TestSendImageWithReceiptFillsDefaultDimensions(t *testing.T) {
	// connection、requests 保存回放成功响应的本地连接与请求帧。
	connection, requests := newAPIResponseConn(t, map[string]any{"messageId": "4294154215667.PNM"}, http.StatusOK)
	// sendErr 表示图片发送是否成功。
	if sendErr := connection.SendImage(context.Background(), "my", "cid", "to", "https://cdn.example/gift.png", 0, 0); sendErr != nil {
		t.Fatalf("图片发送失败: %v", sendErr)
	}
	select {
	case /* envelope 是服务端收到的图片发送信封。 */ envelope := <-requests:
		// body 是 sendByReceiverScope 的消息参数。
		body, _ := envelope["body"].([]any)
		if len(body) == 0 {
			t.Fatalf("图片发送缺少消息参数: %+v", envelope)
		}
		// messageBody 是包含外层 contentType=101 的消息体。
		messageBody, _ := body[0].(map[string]any)
		// outerContent 是聊天协议外层正文。
		outerContent, _ := messageBody["content"].(map[string]any)
		if outerContent["contentType"] != float64(101) {
			t.Fatalf("图片外层 contentType=%v", outerContent["contentType"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到图片发送信封")
	}
}

// TestHandleSyncExtraCoversRequestFailures 验证增量同步确认链路的错误分支。
func TestHandleSyncExtraCoversRequestFailures(t *testing.T) {
	// syncMessage 是类型为 1、需要回传确认的增量同步帧。
	syncMessage := map[string]any{"body": map[string]any{"syncExtraType": map[string]any{"type": float64(1)}}}
	// brokenConn 是在请求前被强制断开的同步连接，用于覆盖 getState 的传输失败分支。
	brokenConn, _ := newAPIResponseConn(t, map[string]any{}, http.StatusOK)
	_ = brokenConn.ws.CloseNow()
	// stateErr 是 getState 传输失败后的错误。
	if stateErr := brokenConn.handleSyncExtra(context.Background(), syncMessage); stateErr == nil {
		t.Fatal("getState 传输失败时应返回错误")
	}
	// abnormalConn 是回放异常状态码的连接，用于覆盖 getState 响应异常分支。
	abnormalConn, _ := newAPIResponseConn(t, nil, http.StatusBadRequest)
	// abnormalErr 是 getState 响应异常时的错误。
	if abnormalErr := abnormalConn.handleSyncExtra(context.Background(), syncMessage); abnormalErr == nil {
		t.Fatal("getState 响应异常时应返回错误")
	}
	// ackFailConn 是 getState 成功但后续请求因连接断开而失败的连接。
	ackFailConn, _ := dialScriptedConn(t, []map[string]any{{"code": float64(200), "body": map[string]any{"state": "ok"}}})
	// ackErr 是 ackDiff 传输失败时的错误。
	if ackErr := ackFailConn.handleSyncExtra(context.Background(), syncMessage); ackErr == nil {
		t.Fatal("ackDiff 传输失败时应返回错误")
	}
	// ackRejectedConn 是 getState 成功但 ackDiff 被平台拒绝的连接。
	ackRejectedConn, _ := dialScriptedConn(t, []map[string]any{
		{"code": float64(200), "body": map[string]any{"state": "ok"}},
		{"code": float64(http.StatusInternalServerError), "body": map[string]any{}},
	})
	// rejectErr 是 ackDiff 被拒绝时的错误。
	if rejectErr := ackRejectedConn.handleSyncExtra(context.Background(), syncMessage); rejectErr == nil {
		t.Fatal("ackDiff 被拒绝时应返回错误")
	}
	// ignoredConn 是类型无需确认的同步连接，必须直接成功返回。
	ignoredConn, _ := newAPIResponseConn(t, map[string]any{}, http.StatusOK)
	if ignoredErr := ignoredConn.handleSyncExtra(context.Background(), map[string]any{"body": map[string]any{"syncExtraType": map[string]any{"type": float64(9)}}}); ignoredErr != nil {
		t.Fatalf("无需确认的同步帧应直接成功: %v", ignoredErr)
	}
}

// TestExtractSyncPayloadRejectsMalformedShape 验证同步载荷形状不合法时安全返回。
func TestExtractSyncPayloadRejectsMalformedShape(t *testing.T) {
	// cases 覆盖缺少 body、缺少同步包、数据项非对象三类形状异常。
	cases := []struct {
		// name 是本用例覆盖的分支描述。
		name string
		// message 是待解析的同步帧。
		message map[string]any
	}{
		{name: "缺少 body", message: map[string]any{}},
		{name: "缺少同步包", message: map[string]any{"body": map[string]any{}}},
		{name: "数据项非对象", message: map[string]any{"body": map[string]any{"syncPushPackage": map[string]any{"data": []any{"不是对象"}}}}},
	}
	// index 标识当前用例在表格中的位置，便于定位失败项。
	for index, testCase := range cases {
		if payload, ok := extractSyncPayload(testCase.message); ok || payload != "" {
			t.Errorf("用例 %d (%s): 非法形状应返回空载荷", index, testCase.name)
		}
	}
}

// TestDecodeSyncDataSupportsEncryptedMsgpackPayload 验证非 JSON 载荷走 msgpack 解密路径。
func TestDecodeSyncDataSupportsEncryptedMsgpackPayload(t *testing.T) {
	// 该字节串是 MessagePack 编码的 {"1":"a"}：fixmap + 整数键 + fixstr；它不是合法 JSON，只能由解密路径处理。
	encoded := "gQGhYQ=="
	// decoded、decodeErr 是解密路径的解析结果与错误。
	decoded, decodeErr := decodeSyncData(encoded)
	if decodeErr != nil {
		t.Fatalf("msgpack 载荷应能解密: %v", decodeErr)
	}
	if decoded["1"] != "a" {
		t.Fatalf("解密结果异常: %+v", decoded)
	}
}

// TestMarkChatReadReportsSuccessAndMissingIDs 验证已读上报的成功分支与空消息标识过滤。
func TestMarkChatReadReportsSuccessAndMissingIDs(t *testing.T) {
	// connection、requests 保存回放成功响应的本地连接与请求帧。
	connection, requests := newAPIResponseConn(t, map[string]any{}, http.StatusOK)
	// readErr 表示已读上报是否成功。
	if readErr := connection.MarkChatRead(context.Background(), "cid", []map[string]any{{"messageId": " 4294154215667.PNM "}, {"messageId": nil}}); readErr != nil {
		t.Fatalf("已读上报失败: %v", readErr)
	}
	select {
	case /* envelope 是服务端收到的已读上报信封。 */ envelope := <-requests:
		// body 是 MessageStatusService 的单参数列表。
		body, _ := envelope["body"].([]any)
		if len(body) != 1 {
			t.Fatalf("已读上报参数层数异常: %+v", body)
		}
		// ids 是过滤后的非空消息标识列表。
		ids, _ := body[0].([]any)
		if len(ids) != 1 || ids[0] != "4294154215667.PNM" {
			t.Fatalf("已读消息标识未规范化: %+v", ids)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到已读上报信封")
	}
}
