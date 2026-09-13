package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/internal/imageproc"
)

// sizedPNGBase64 生成指定尺寸的纯色 PNG,模拟上游返回的基础分辨率图。
func sizedPNGBase64(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func decodePNGSize(t *testing.T, b64 string) (int, int) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode image config: %v", err)
	}
	return cfg.Width, cfg.Height
}

func imagesCompletedSSE(resultB64, size string) string {
	return `data: {"type":"response.completed","response":{"created_at":1710000000,"usage":{"input_tokens":5,"output_tokens":9},"tool_usage":{"image_gen":{"images":1,"input_tokens":34,"output_tokens":1756}},"tools":[{"type":"image_generation","model":"gpt-image-2","output_format":"png","quality":"low","size":"` + size + `"}],"output":[{"type":"image_generation_call","result":"` + resultB64 + `","revised_prompt":"draw a cat","output_format":"png"}]}}` + "\n\n"
}

func TestImageUpscalePlanForRequest(t *testing.T) {
	body := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"2048x2048"}]}`)

	plan := imageUpscalePlanForRequest("gpt-image-2-2k", "gpt-image-2-2k", body, "")
	if plan.Scale != "2k" || plan.RequestedSize != "2048x2048" {
		t.Fatalf("2k plan = %#v", plan)
	}
	plan = imageUpscalePlanForRequest("GPT-Image-2-4K", "GPT-Image-2-4K", body, "")
	if plan.Scale != "4k" {
		t.Fatalf("4k plan = %#v", plan)
	}
	if plan = imageUpscalePlanForRequest("gpt-image-2", "gpt-image-2", body, ""); plan.enabled() {
		t.Fatalf("plain model should not enable upscale, got %#v", plan)
	}
	if plan = imageUpscalePlanForRequest("gpt-image-1.5", "gpt-image-1.5", body, ""); plan.enabled() {
		t.Fatalf("other model should not enable upscale, got %#v", plan)
	}

	autoBody := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"auto"}]}`)
	if plan = imageUpscalePlanForRequest("gpt-image-2-2k", "gpt-image-2-2k", autoBody, ""); plan.RequestedSize != "" {
		t.Fatalf("auto size should clear RequestedSize, got %#v", plan)
	}
}

// 2K 别名:上游基础图 512x512,最终返回必须是物理 2048x2048(issue #477)。
func TestCollectImagesResponseUpscales2KAlias(t *testing.T) {
	upstream := imagesCompletedSSE(sizedPNGBase64(t, 512, 512), "2048x2048")
	plan := imageUpscalePlan{Scale: "2k", RequestedSize: "2048x2048"}

	out, _, imageCount, imageLogInfo, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2-2k", nil, plan)
	if err != nil {
		t.Fatalf("collectImagesResponse returned error: %v", err)
	}
	if imageCount != 1 {
		t.Fatalf("imageCount = %d, want 1", imageCount)
	}
	width, height := decodePNGSize(t, gjson.GetBytes(out, "data.0.b64_json").String())
	if width != 2048 || height != 2048 {
		t.Fatalf("physical size = %dx%d, want 2048x2048", width, height)
	}
	if got := gjson.GetBytes(out, "data.0.width").Int(); got != 2048 {
		t.Fatalf("data.0.width = %d, want 2048", got)
	}
	if imageLogInfo.Width != 2048 || imageLogInfo.Height != 2048 {
		t.Fatalf("imageLogInfo size = %dx%d, want 2048x2048", imageLogInfo.Width, imageLogInfo.Height)
	}
}

// 4K 横图:上游 16:9 基础图要被拉到精确 3840x2160。
func TestCollectImagesResponseUpscales4KLandscape(t *testing.T) {
	upstream := imagesCompletedSSE(sizedPNGBase64(t, 640, 360), "3840x2160")
	plan := imageUpscalePlan{Scale: "4k", RequestedSize: "3840x2160"}

	out, _, _, _, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2-4k", nil, plan)
	if err != nil {
		t.Fatalf("collectImagesResponse returned error: %v", err)
	}
	width, height := decodePNGSize(t, gjson.GetBytes(out, "data.0.b64_json").String())
	if width != 3840 || height != 2160 {
		t.Fatalf("physical size = %dx%d, want 3840x2160", width, height)
	}
}

// 用户显式传入的非预设合法尺寸是权威目标,不被别名默认值覆盖。
func TestCollectImagesResponseHonorsExplicitCustomSize(t *testing.T) {
	upstream := imagesCompletedSSE(sizedPNGBase64(t, 400, 400), "1600x1600")
	plan := imageUpscalePlan{Scale: "4k", RequestedSize: "1600x1600"}

	out, _, _, _, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2-4k", nil, plan)
	if err != nil {
		t.Fatalf("collectImagesResponse returned error: %v", err)
	}
	width, height := decodePNGSize(t, gjson.GetBytes(out, "data.0.b64_json").String())
	if width != 1600 || height != 1600 {
		t.Fatalf("physical size = %dx%d, want explicit 1600x1600 not the 4K tier box", width, height)
	}
}

func TestCollectImagesResponsePadsMismatchedSourceToExactCustomSize(t *testing.T) {
	upstream := imagesCompletedSSE(sizedPNGBase64(t, 4, 4), "12x6")
	plan := imageUpscalePlan{Scale: "2k", RequestedSize: "12x6"}

	out, _, _, _, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2-2k", nil, plan)
	if err != nil {
		t.Fatalf("collectImagesResponse returned error: %v", err)
	}
	width, height := decodePNGSize(t, gjson.GetBytes(out, "data.0.b64_json").String())
	if width != 12 || height != 6 {
		t.Fatalf("physical size = %dx%d, want exact 12x6 canvas", width, height)
	}
}

// 上游已达标时不重编码,原始字节原样返回。
func TestApplyImageUpscalePlanSkipsWhenAlreadyAtTarget(t *testing.T) {
	b64 := sizedPNGBase64(t, 2048, 2048)
	results := applyImageUpscalePlan(context.Background(), imageUpscalePlan{Scale: "2k", RequestedSize: "2048x2048"}, []imageCallResult{{Result: b64}})
	if results[0].Result != b64 {
		t.Fatal("image already at target size should pass through unchanged")
	}
}

// 超分失败(不可解码的图)降级返回上游原图,而不是让整个请求失败。
func TestApplyImageUpscalePlanFailureFallsBack(t *testing.T) {
	notImage := base64.StdEncoding.EncodeToString([]byte("definitely not an image payload"))
	results := applyImageUpscalePlan(context.Background(), imageUpscalePlan{Scale: "4k", RequestedSize: "3840x2160"}, []imageCallResult{{Result: notImage, Width: 3, Height: 4}})
	if results[0].Result != notImage || results[0].Width != 3 || results[0].Height != 4 {
		t.Fatalf("failed upscale should keep the original result, got %#v", results[0])
	}
}

// 未启用计划(普通 gpt-image-2)完全不动结果。
func TestApplyImageUpscalePlanDisabledNoop(t *testing.T) {
	b64 := sizedPNGBase64(t, 64, 64)
	results := applyImageUpscalePlan(context.Background(), imageUpscalePlan{}, []imageCallResult{{Result: b64}})
	if results[0].Result != b64 {
		t.Fatal("disabled plan must not touch results")
	}
}

// 流式路径:completed 事件里的最终图也要是目标物理尺寸。
func TestStreamImagesResponseUpscalesCompletedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := imagesCompletedSSE(sizedPNGBase64(t, 256, 256), "2048x2048")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
	handler := &Handler{}
	plan := imageUpscalePlan{Scale: "2k", RequestedSize: "2048x2048"}

	_, imageCount, _, imageLogInfo, wroteImageOutput, err := handler.streamImagesResponse(c, strings.NewReader(upstream), "b64_json", "image_generation", "gpt-image-2-2k", time.Now(), plan)
	if err != nil {
		t.Fatalf("streamImagesResponse returned error: %v", err)
	}
	if imageCount != 1 {
		t.Fatalf("imageCount = %d, want 1", imageCount)
	}
	if !wroteImageOutput {
		t.Fatal("wroteImageOutput = false, want true after completed image event")
	}
	if imageLogInfo.Width != 2048 || imageLogInfo.Height != 2048 {
		t.Fatalf("imageLogInfo size = %dx%d, want 2048x2048", imageLogInfo.Width, imageLogInfo.Height)
	}

	body := recorder.Body.String()
	completedIdx := strings.Index(body, "event: image_generation.completed\ndata: ")
	if completedIdx < 0 {
		t.Fatalf("stream body missing completed event: %q", body)
	}
	payload := body[completedIdx+len("event: image_generation.completed\ndata: "):]
	if end := strings.Index(payload, "\n\n"); end >= 0 {
		payload = payload[:end]
	}
	width, height := decodePNGSize(t, gjson.Get(payload, "b64_json").String())
	if width != 2048 || height != 2048 {
		t.Fatalf("streamed physical size = %dx%d, want 2048x2048", width, height)
	}
}

// 公共 Images API 常传 1920x1080 这类显示尺寸。GPT Image 2 只接受 16 的倍数，
// 网关必须向上取整后再发上游，并把用户要的精确画布留给后处理，而不是直接 400。
func TestPreparePublicImagesRequestRoundsUpstreamSizeAndKeepsExactCanvas(t *testing.T) {
	body := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"1920x1080"}]}`)
	prepared, plan, err := preparePublicImagesRequest("gpt-image-2", "gpt-image-2", body, "1920x1080")
	if err != nil {
		t.Fatalf("preparePublicImagesRequest() error = %v", err)
	}
	if got := gjson.GetBytes(prepared, "tools.0.size").String(); got != "1920x1088" {
		t.Fatalf("upstream size = %q, want 1920x1088 (1080 向上取整到 16 的倍数)", got)
	}
	if plan.RequestedSize != "1920x1080" {
		t.Fatalf("plan.RequestedSize = %q, want 1920x1080", plan.RequestedSize)
	}
	if plan.Scale != "" {
		t.Fatalf("plan.Scale = %q, want empty (无 -2k/-4k 别名)", plan.Scale)
	}
	if !plan.enabled() {
		t.Fatal("plan.enabled() = false, 精确画布必须触发后处理缩放")
	}
}

// 已经是 16 倍数的尺寸不该被改写；auto / 空值保持原样且不生成计划。
func TestPreparePublicImagesRequestLeavesAlignedAndAutoSizes(t *testing.T) {
	aligned := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"1024x1024"}]}`)
	prepared, plan, err := preparePublicImagesRequest("gpt-image-2", "gpt-image-2", aligned, "1024x1024")
	if err != nil {
		t.Fatalf("aligned error = %v", err)
	}
	if got := gjson.GetBytes(prepared, "tools.0.size").String(); got != "1024x1024" {
		t.Fatalf("aligned upstream size = %q, want 1024x1024", got)
	}
	if plan.RequestedSize != "1024x1024" {
		t.Fatalf("aligned plan.RequestedSize = %q, want 1024x1024", plan.RequestedSize)
	}

	autoBody := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"auto"}]}`)
	prepared, plan, err = preparePublicImagesRequest("gpt-image-2", "gpt-image-2", autoBody, "auto")
	if err != nil {
		t.Fatalf("auto error = %v", err)
	}
	if got := gjson.GetBytes(prepared, "tools.0.size").String(); got != "auto" {
		t.Fatalf("auto upstream size = %q, want auto", got)
	}
	if plan.enabled() {
		t.Fatalf("auto 不该生成后处理计划, got %#v", plan)
	}
}

// 超出比例/像素预算的尺寸仍要被拒；单边超限不能因为乘法溢出而漏网。
func TestPreparePublicImagesRequestRejectsOutOfBudgetSizes(t *testing.T) {
	body := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"1024x1024"}]}`)
	for _, size := range []string{"4000x100", "99999999999999x16"} {
		if _, _, err := preparePublicImagesRequest("gpt-image-2", "gpt-image-2", body, size); err == nil {
			t.Fatalf("size %q 应被拒绝", size)
		}
	}
}

// 用户写的 -2k/-4k 别名优先于映射；映射到别名上的自定义模型继承该档位。
func TestImageUpscalePlanForRequestResolvesTierFromEitherModel(t *testing.T) {
	body := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"2048x2048"}]}`)
	if plan := imageUpscalePlanForRequest("gpt-image-2", "gpt-image-2-4k", body, ""); plan.Scale != imageproc.Upscale4K {
		t.Fatalf("用户显式别名未生效: %#v", plan)
	}
	if plan := imageUpscalePlanForRequest("gpt-image-2-2k", "my-image", body, ""); plan.Scale != imageproc.Upscale2K {
		t.Fatalf("映射到别名的自定义模型未继承档位: %#v", plan)
	}
}

// 显式画布要变成构图硬要求，避免托管工具把宽幅请求画成两侧留白的竖版海报。
func TestAppendNativeImageCanvasInstruction(t *testing.T) {
	got := appendNativeImageCanvasInstruction("a cat", "1920x1080")
	for _, want := range []string{"a cat", "horizontal composition", "1920x1080", "vertical poster"} {
		if !strings.Contains(got, want) {
			t.Fatalf("宽幅指令缺少 %q: %q", want, got)
		}
	}
	if got := appendNativeImageCanvasInstruction("a cat", "1024x2048"); !strings.Contains(got, "tall vertical") {
		t.Fatalf("高窄画布应给竖版指令: %q", got)
	}
	// 没有显式尺寸 / 尺寸非法时不得改写 prompt。
	for _, size := range []string{"", "auto", "not-a-size"} {
		if got := appendNativeImageCanvasInstruction("a cat", size); got != "a cat" {
			t.Fatalf("size %q 不该改写 prompt, got %q", size, got)
		}
	}
}

// tool_choice 点名 image_gen namespace 与扁平 image_generation 等价，都是明确的生图意图；
// 仅在 tools[] 里被无差别注入的声明仍不算意图（issue #304）。
func TestImageGenerationToolChoiceRecognizesNamespace(t *testing.T) {
	forced := [][]byte{
		[]byte(`{"model":"gpt-5.5","tool_choice":{"type":"namespace","name":"image_gen"}}`),
		[]byte(`{"model":"gpt-5.5","tool_choice":"image_gen"}`),
		[]byte(`{"model":"gpt-5.5","tool_choice":{"type":"image_generation"}}`),
	}
	for _, body := range forced {
		if !rawResponsesBodyShouldForceHTTPForImageGeneration(body) {
			t.Fatalf("tool_choice 点名生图应强制 HTTP: %s", body)
		}
	}
	injected := []byte(`{"model":"gpt-5.5","input":"hello","tools":[{"type":"namespace","name":"image_gen"}]}`)
	if rawResponsesBodyShouldForceHTTPForImageGeneration(injected) {
		t.Fatalf("仅声明 namespace 而无 tool_choice/自然语言意图时不得强制 HTTP: %s", injected)
	}
}
