import pytest

from codebuddy2api.pool import AccountPool
from codebuddy2api.schemas import AccountCreate
from codebuddy2api.store import AccountStore


@pytest.mark.asyncio
async def test_pool_respects_concurrency(tmp_path):
    store = AccountStore(str(tmp_path / "test.sqlite3"))
    store.add_account(AccountCreate(name="a", api_key="ck_test_1", concurrency=1))
    pool = AccountPool(store)

    first = await pool.acquire()
    with pytest.raises(Exception):
        await pool.acquire()
    await pool.release(first)
    second = await pool.acquire()
    assert second.account.id == first.account.id
    await pool.release(second)


@pytest.mark.asyncio
async def test_pool_prefers_higher_priority_until_saturated(tmp_path):
    store = AccountStore(str(tmp_path / "test.sqlite3"))
    low_id = store.add_account(AccountCreate(name="low", api_key="ck_test_1", priority=10, concurrency=5))
    high_id = store.add_account(AccountCreate(name="high", api_key="ck_test_2", priority=100, concurrency=1))
    pool = AccountPool(store)

    first = await pool.acquire()
    assert first.account.id == high_id

    second = await pool.acquire()
    assert second.account.id == low_id

    await pool.release(first)
    third = await pool.acquire()
    assert third.account.id == high_id

    await pool.release(second)
    await pool.release(third)


def test_store_records_credit(tmp_path):
    store = AccountStore(str(tmp_path / "test.sqlite3"))
    account_id = store.add_account(AccountCreate(name="a", api_key="ck_test_1"))
    store.record_success(account_id, {"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5, "credit": 0.25})
    stats = store.stats()
    assert stats["total_requests"] == 1
    assert stats["total_credit"] == 0.25


def test_store_deletes_account(tmp_path):
    store = AccountStore(str(tmp_path / "test.sqlite3"))
    account_id = store.add_account(AccountCreate(name="a", api_key="ck_test_1"))

    assert store.delete_account(account_id) is True
    assert store.get_account(account_id) is None
    assert store.delete_account(account_id) is False
