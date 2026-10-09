#!/usr/bin/env python3
"""THROWAWAY: TCP proxy adding RTT/2 one-way delay per direction (latency only,
order preserved). RTT in ms is re-read from a file per chunk so it can change
between runs. usage: delayproxy.py LISTEN_PORT TARGET_PORT RTT_FILE"""
import asyncio, sys, time

LP, TP, RTT_FILE = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]

def half():
    try:
        return float(open(RTT_FILE).read().strip()) / 2000.0
    except Exception:
        return 0.0

async def pump(reader, writer):
    q = asyncio.Queue()
    async def deliver():
        while True:
            t, data = await q.get()
            if data is None:
                break
            d = t - time.monotonic()
            if d > 0:
                await asyncio.sleep(d)
            writer.write(data)
            await writer.drain()
        try:
            writer.write_eof()
        except Exception:
            pass
    task = asyncio.create_task(deliver())
    try:
        while True:
            data = await reader.read(1 << 16)
            if not data:
                break
            await q.put((time.monotonic() + half(), data))
    finally:
        await q.put((0, None))
        await task

async def handle(cr, cw):
    try:
        ur, uw = await asyncio.open_connection("127.0.0.1", TP)
    except Exception:
        cw.close(); return
    await asyncio.gather(pump(cr, uw), pump(ur, cw), return_exceptions=True)
    for w in (cw, uw):
        try: w.close()
        except Exception: pass

async def main():
    srv = await asyncio.start_server(handle, "127.0.0.1", LP)
    async with srv:
        await srv.serve_forever()

asyncio.run(main())
