"""适配器加载的单测（不触网、不启动浏览器）。

修的是 2026-11-13 实测到的死洞：加载器按固定名字 ``getattr(module, "Adapter")``
取类，而三个实现里分别叫 ``LongcatAdapter`` / ``CompshareAdapter`` /
``SensenovaAdapter``，于是三家全部 503 ``adapter not implemented`` ——
「控制台用量永远读不到」的真根因就是这个，跟抓取逻辑本身无关。

契约：模块里那个 BaseAdapter 子类就是适配器，类叫什么名字与加载无关。
"""

import asyncio
import importlib
import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import app as app_module  # noqa: E402
from adapters.base import BaseAdapter, PROVIDERS  # noqa: E402


@pytest.mark.parametrize("provider", sorted(PROVIDERS))
def test_every_declared_provider_loads(provider):
    """凡是声明支持的 provider，都必须真的加载得到适配器。"""
    adapter = asyncio.run(app_module.get_adapter(provider))
    assert isinstance(adapter, BaseAdapter)
    assert adapter.provider == provider


def test_adapter_class_name_is_not_part_of_the_contract():
    """实现类换名字不能让加载失效（旧实现在这里直接 AttributeError）。"""
    module = importlib.import_module("adapters.longcat")
    assert not hasattr(module, "Adapter"), "该用例前提：模块里没有叫 Adapter 的类"
    cls = app_module._resolve_adapter_class(module)
    assert cls.__name__ == "LongcatAdapter"


def test_missing_adapter_module_is_reported_not_swallowed():
    """真的没实现时要报 adapter not implemented，不能假装成功。"""
    with pytest.raises(app_module.AdapterNotImplemented):
        asyncio.run(app_module.get_adapter("no_such_provider"))
