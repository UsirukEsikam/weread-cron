package weread

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDifferentialAgainstOfficialJS 是与官方微信读书 Web Reader 脚本解混淆参考实现的一次性
// 差分校验（开发期工具，非 CI 断言）：读取 /tmp/weread-ref/diff_cases.json（由
// /tmp/weread-ref/diff.js + ref.js 生成——ref.js 内含从官方 bundle 9.898844ac.js 提取的
// 原版 sign/buildQuery/appId 与经真实 golden 向量验证的 _e），存在时运行 389 个随机用例
// （含中文/emoji/零宽字符/控制字符/超长值），不存在时跳过。
//
// 本包提交的确定性 golden 向量（protocol_test.go）与这些随机用例同源。
func TestDifferentialAgainstOfficialJS(t *testing.T) {
	raw, err := os.ReadFile("/tmp/weread-ref/diff_cases.json")
	if err != nil {
		t.Skip("差分用例不存在（/tmp/weread-ref/diff_cases.json）；golden 向量见 protocol_test.go")
	}
	var cases struct {
		Sign []struct {
			Params map[string]string `json:"params"`
			Want   string            `json:"want"`
		} `json:"sign"`
		Encode []struct {
			In   string `json:"in"`
			Want string `json:"want"`
		} `json:"encode"`
		AppID []struct {
			UA   string `json:"ua"`
			Want string `json:"want"`
		} `json:"appId"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("解析差分用例失败: %v", err)
	}
	for i, c := range cases.Sign {
		if got := SignPayload(c.Params); got != c.Want {
			t.Errorf("sign case %d: got %s, want %s", i, got, c.Want)
		}
	}
	for i, c := range cases.Encode {
		if got := EncodeID(c.In); got != c.Want {
			t.Errorf("encode case %d (%q): got %s, want %s", i, c.In, got, c.Want)
		}
	}
	for i, c := range cases.AppID {
		if got := AppID(c.UA); got != c.Want {
			t.Errorf("appId case %d: got %s, want %s", i, got, c.Want)
		}
	}
}
