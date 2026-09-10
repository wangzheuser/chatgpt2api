"""图片模型默认值和新旧名称兼容性回归。"""
import unittest
from unittest import mock

from api.ai import ImageGenerationRequest
from api.image_tasks import ImageGenerationTaskRequest
from services.config import config
from services.openai_backend_api import OpenAIBackendAPI
from services.protocol import openai_v1_models, openai_v1_image_generations, openai_v1_image_edit
from utils.helper import split_image_model, is_codex_image_model


class ImageModelDefaultsTests(unittest.TestCase):
    def test_supported_names_and_api_defaults(self):
        """新名称默认启用，旧名称和 Codex 语义保留。"""
        for name in ("gpt-image-2", "gpt-image-2-5"):
            self.assertEqual(split_image_model(name), (None, name))
            self.assertFalse(is_codex_image_model(name))
        self.assertEqual(split_image_model("gpt-image-2.5"), (None, None))
        self.assertTrue(is_codex_image_model("plus-codex-gpt-image-2"))
        self.assertEqual(ImageGenerationRequest(prompt="test").model, "gpt-image-2-5")
        self.assertEqual(ImageGenerationTaskRequest(prompt="test", client_task_id="test").model, "gpt-image-2-5")

    def test_http_generation_and_multipart_edit_defaults(self):
        """真实路由解析省略模型的请求，生成与编辑均使用新默认值。"""
        from fastapi import FastAPI
        from fastapi.testclient import TestClient
        import api.ai as ai
        app = FastAPI()
        app.include_router(ai.create_router())
        client = TestClient(app)
        with mock.patch.dict(config.data, {"auth-key": "chatgpt2api"}), mock.patch.object(
            ai, "filter_or_log", mock.AsyncMock()
        ), mock.patch.object(ai.openai_v1_image_generations, "handle", return_value={"created": 1, "data": []}) as generate, mock.patch.object(
            ai.openai_v1_image_edit, "handle", return_value={"created": 1, "data": []}
        ) as edit:
            headers = {"Authorization": "Bearer chatgpt2api"}
            result = client.post("/v1/images/generations", headers=headers, json={"prompt": "test"})
            self.assertEqual(result.status_code, 200, result.text)
            self.assertEqual(generate.call_args.args[0]["model"], "gpt-image-2-5")
            result = client.post("/v1/images/edits", headers=headers, data={"prompt": "test"},
                                 files={"image": ("test.png", b"image", "image/png")})
            self.assertEqual(result.status_code, 200, result.text)
            self.assertEqual(edit.call_args.args[0]["model"], "gpt-image-2-5")

    def test_catalog_adds_both_names_without_duplicates(self):
        """列表沿用相同账号条件，并保留去重行为。"""
        for accounts in ([], [{"source_type": "web"}]):
            with mock.patch.object(openai_v1_models.account_service, "list_accounts", return_value=accounts), mock.patch.object(
                openai_v1_models.model_catalog_service, "list_models",
                side_effect=lambda: {"object": "list", "data": []},
            ):
                ids = [item["id"] for item in openai_v1_models.list_models()["data"]]
                self.assertEqual(set(ids), {"gpt-image-2", "gpt-image-2-5"} if accounts else set())
            with mock.patch.object(openai_v1_models.account_service, "list_accounts", return_value=accounts), mock.patch.object(
                openai_v1_models.model_catalog_service, "list_models",
                return_value={"data": [{"id": "gpt-image-2-5"}]},
            ):
                ids = [item["id"] for item in openai_v1_models.list_models()["data"]]
                self.assertEqual(ids.count("gpt-image-2-5"), 1)

    def test_upstream_config_and_both_request_stages(self):
        """配置优先，空值回退；两个上游阶段使用同一模型和思考强度。"""
        backend = OpenAIBackendAPI.__new__(OpenAIBackendAPI)
        backend.base_url = "https://example.invalid"
        backend.session = mock.Mock()
        backend.session.post.return_value.status_code = 200
        backend.session.post.return_value.json.return_value = {"conduit_token": "test"}
        backend._image_headers = mock.Mock(return_value={})
        for model in ("gpt-image-2", "gpt-image-2-5"):
            for value, expected, effort in (
                (None, "gpt-5-6", "standard"),
                ("", "gpt-5-6", "standard"),
                ("   ", "gpt-5-6", "standard"),
                ("gpt-5-5", "gpt-5-5", "standard"),
                (" custom-model ", "custom-model", "standard"),
                ("custom-model-max", "custom-model", "max"),
            ):
                with self.subTest(model=model, value=value), mock.patch.dict(config.data, {"default_thinking_effort": "standard"}, clear=True):
                    if value is not None:
                        config.data["default_upstream_model_name"] = value
                    self.assertEqual(backend._image_model_settings(model), (expected, effort))
                    backend.session.reset_mock()
                    backend._prepare_image_conversation("test", None, model)
                    backend._start_image_generation("test", None, "test", model, [])
                    for call in backend.session.post.call_args_list:
                        self.assertEqual(call.kwargs["json"]["model"], expected)
                        self.assertEqual(call.kwargs["json"]["thinking_effort"], effort)

    def test_generation_and_edit_share_pool_with_new_default(self):
        """生成与编辑的流式入口共享账号池，并保留显式旧模型。"""
        for module in (openai_v1_image_generations, openai_v1_image_edit):
            for explicit in (None, "gpt-image-2", "gpt-image-2-5"):
                body = {"prompt": "test", "images": [(b"image", "test.png", "image/png")], "stream": True}
                if explicit is not None:
                    body["model"] = explicit
                with mock.patch.object(module, "stream_image_outputs_with_pool", return_value=iter(())) as pool, mock.patch.object(
                    module, "stream_image_chunks", return_value=iter(())
                ):
                    list(module.handle(body))
                    self.assertEqual(pool.call_args.args[0].model, explicit or "gpt-image-2-5")


if __name__ == "__main__":
    unittest.main()
