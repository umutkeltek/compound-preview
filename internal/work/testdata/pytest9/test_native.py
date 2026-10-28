"""Synthetic fixture for genuine pytest 9.0.3 xunit1 report capture."""
import pytest

@pytest.mark.parametrize("value", [1, 2], ids=["alpha", "beta"])
def test_values(value):
    assert value > 0

class TestGroup:
    def test_nested(self):
        assert 2 + 2 == 4

@pytest.mark.skip(reason="optional native fixture")
def test_optional():
    assert False
