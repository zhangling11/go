#!/usr/bin/env python3
"""Probe a service inside the guest from an independent client and save CSV."""
import argparse
import csv
from datetime import datetime, timezone
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


def main():
    parser = argparse.ArgumentParser(description="记录虚拟机 HTTP 探测结果")
    parser.add_argument("url", help="虚拟机内的 HTTP 健康检查地址")
    parser.add_argument("--output", default="business-probe.csv")
    parser.add_argument("--interval", type=float, default=0.2, help="发起间隔（秒）")
    parser.add_argument("--timeout", type=float, default=0.5, help="HTTP 超时（秒）")
    args = parser.parse_args()
    if args.interval <= 0 or args.timeout <= 0:
        parser.error("间隔和超时必须大于 0")

    last_success = None
    first_failure = None
    failures = 0
    with open(args.output, "x", newline="", encoding="utf-8") as output:
        writer = csv.writer(output)
        writer.writerow(["started_utc", "duration_ms", "status", "result",
                         "failed_probes", "first_failed_start_to_recovery_ms",
                         "last_success_to_recovery_ms"])
        print(f"正在记录到 {args.output}，按 Ctrl+C 结束。")
        try:
            while True:
                started = time.monotonic()
                timestamp = datetime.now(timezone.utc).isoformat(timespec="milliseconds")
                status = ""
                result = "ok"
                try:
                    request = Request(args.url, headers={"Cache-Control": "no-cache"})
                    with urlopen(request, timeout=args.timeout) as response:
                        status = response.status
                except HTTPError as error:
                    status = error.code
                    result = "http_error"
                except (URLError, OSError) as error:
                    result = str(error)
                completed = time.monotonic()
                failure_window = success_gap = ""
                run_failures = ""
                if result != "ok":
                    if first_failure is None:
                        first_failure = started
                    failures += 1
                else:
                    if first_failure is not None:
                        run_failures = failures
                        failure_window = round((completed - first_failure) * 1000, 1)
                        if last_success is not None:
                            success_gap = round((completed - last_success) * 1000, 1)
                        print(f"HTTP 恢复。连续失败 {failures} 次，首个失败至成功 {failure_window} ms")
                        first_failure = None
                        failures = 0
                    last_success = completed
                writer.writerow([timestamp, round((completed - started) * 1000, 1),
                                 status, result, run_failures, failure_window, success_gap])
                output.flush()
                time.sleep(max(0, args.interval - (time.monotonic() - started)))
        except KeyboardInterrupt:
            if failures:
                print(f"探测结束时仍有 {failures} 次连续失败，尚未观测到恢复。")


if __name__ == "__main__":
    main()
