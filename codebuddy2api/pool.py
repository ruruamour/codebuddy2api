from __future__ import annotations

import asyncio
from collections import defaultdict
from dataclasses import dataclass

from .store import Account, AccountStore


class NoAccountAvailable(RuntimeError):
    pass


@dataclass(frozen=True)
class Lease:
    account: Account


class AccountPool:
    def __init__(self, store: AccountStore):
        self.store = store
        self._lock = asyncio.Lock()
        self._in_flight: dict[int, int] = defaultdict(int)
        self._rr_cursor = 0

    async def acquire(self) -> Lease:
        async with self._lock:
            candidates = self.store.schedulable_accounts()
            if not candidates:
                raise NoAccountAvailable("no enabled CodeBuddy accounts available")

            for priority in sorted({account.priority for account in candidates}, reverse=True):
                weighted = _weighted_candidates(
                    [account for account in candidates if account.priority == priority]
                )
                total = len(weighted)
                for offset in range(total):
                    index = (self._rr_cursor + offset) % total
                    account = weighted[index]
                    if self._in_flight[account.id] >= account.concurrency:
                        continue
                    self._in_flight[account.id] += 1
                    self._rr_cursor = (index + 1) % total
                    return Lease(account=account)

            raise NoAccountAvailable("all CodeBuddy accounts are at concurrency limit")

    async def release(self, lease: Lease) -> None:
        async with self._lock:
            current = self._in_flight.get(lease.account.id, 0)
            if current <= 1:
                self._in_flight.pop(lease.account.id, None)
            else:
                self._in_flight[lease.account.id] = current - 1

    async def in_flight_snapshot(self) -> dict[int, int]:
        async with self._lock:
            return dict(self._in_flight)


def _weighted_candidates(accounts: list[Account]) -> list[Account]:
    result: list[Account] = []
    for account in sorted(accounts, key=lambda item: item.id):
        result.extend([account] * max(1, account.weight))
    return result
