#!/usr/bin/env python3
"""显式连接本机 Nagare，跨作品验证找源与 mpv 进度；报告不保存媒体地址、凭据或响应正文。"""

import argparse
import datetime
import json
import queue
import re
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', action='append', required=True, help='animego/AniList ID:正集号')
    parser.add_argument('--port', type=int, default=8591)
    parser.add_argument('--config', type=Path, default=Path.home() / 'Library/Application Support/nagare/config.toml')
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--play', action='store_true', help='启动 mpv 并检查至少 15 秒进度，可能切换当前播放')
    args = parser.parse_args()
    token = re.search(r'token\s*=\s*"([^"]+)"', args.config.read_text())[1]
    headers = {'X-Nagare-Token': token, 'Content-Type': 'application/json'}
    base = f'http://127.0.0.1:{args.port}'

    def request(path, body=None, timeout=20):
        return urllib.request.urlopen(urllib.request.Request(
            base + path, headers=headers,
            data=None if body is None else json.dumps(body).encode()), timeout=timeout)

    def api(path, body=None):
        with request(path, body) as response:
            return json.load(response).get('data', {})

    report = {'startedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'playbackChecked': args.play, 'cases': []}
    plugin = api('/api/source-plugin')
    report['enabledSources'] = [s['id'] for s in plugin.get('sources', []) if s.get('enabled')]

    def save():
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')

    for spec in args.case:
        anime_id, episode = map(int, spec.split(':'))
        case = {'id': anime_id, 'episode': episode, 'errors': [], 'attempts': [], 'result': 'running'}
        report['cases'].append(case)
        save()
        stream = None
        try:
            media = api(f'/api/anime/{anime_id}')
            case['title'] = media['title']
            titles = list(dict.fromkeys(media[k] for k in ('title', 'titleNative', 'titleEnglish') if media.get(k)))
            subject = {'ids': {'anilist': str(anime_id)}, 'titles': titles}
            if media.get('year'):
                subject['year'] = media['year']
            body = {'schema': 'nagare-resolve-request/v1', 'subject': subject,
                    'episode': {'number': str(episode), 'absolute': episode},
                    'preferences': {'preferredTransports': ['hls', 'http', 'torrent']}}
            print('START', case['title'], episode, flush=True)
            started = time.monotonic()
            stream = request('/api/source-plugin/candidates', body, timeout=110)
            events = queue.Queue()

            def read_events(response, output):
                try:
                    for line in response:
                        output.put(json.loads(line))
                except Exception:
                    output.put({'event': 'stream_failed'})
                finally:
                    output.put({'event': 'stream_closed'})

            threading.Thread(target=read_events, args=(stream, events), daemon=True).start()
            while time.monotonic() - started < 140:
                try:
                    event = events.get(timeout=2)
                except queue.Empty:
                    continue
                kind = event.get('event')
                if kind == 'source_error':
                    case['errors'].append({'source': event.get('sourceId'), 'category': event.get('category')})
                elif kind in ('done', 'stream_closed', 'stream_failed'):
                    case['result'] = 'no_playback' if args.play else 'no_candidate'
                    break
                elif kind == 'candidate':
                    candidate = event['candidate']
                    attempt = {'source': candidate['sourceId'], 'transport': candidate['transport']['type'],
                               'matchedTitle': candidate.get('match', {}).get('subjectTitle'),
                               'matchedEpisode': candidate.get('match', {}).get('episodeNumber'),
                               'candidateSeconds': round(time.monotonic() - started, 1)}
                    case['attempts'].append(attempt)
                    if attempt['matchedEpisode'] != episode:
                        attempt['result'] = 'episode_mismatch'
                        continue
                    if not args.play:
                        case['result'] = 'candidate'
                        break
                    if attempt['transport'] not in ('hls', 'http'):
                        attempt['result'] = 'bt_not_tested'
                        continue
                    try:
                        file_id = None
                        play = api('/api/source-plugin/play', {'candidate': candidate, 'title': case['title'], 'episode': episode})
                        file_id = play['fileId']
                        previous = None
                        stable_since = None
                        progressed = 0.0
                        for _ in range(25):
                            time.sleep(2)
                            status = api('/api/player/status')
                            if status.get('playbackFailure', {}).get('fileId') == file_id:
                                attempt['result'] = 'player_failed'
                                break
                            if status.get('fileId') != file_id or not status.get('playing'):
                                attempt['result'] = 'player_stopped'
                                break
                            now = time.monotonic()
                            position = status.get('position', 0)
                            if previous is not None:
                                delta = position - previous[0]
                                elapsed = now - previous[1]
                                # 恢复历史进度或手动跳转不能冒充连续播放。
                                if delta < 0 or delta > elapsed * 2 + 2:
                                    stable_since, progressed = None, 0.0
                                elif delta > 0:
                                    if stable_since is None:
                                        stable_since = previous[1]
                                    progressed += delta
                            previous = (position, now)
                            if status.get('duration', 0) > 0 and stable_since is not None and now - stable_since >= 15 and progressed >= 15:
                                attempt.update(result='progress_verified', duration=round(status['duration'], 1),
                                               progressSeconds=round(progressed, 1), continuousSeconds=round(now - stable_since, 1))
                                case['result'] = 'progress_verified'
                                break
                        else:
                            attempt['result'] = 'progress_timeout'
                    except Exception as exc:
                        attempt['result'] = 'play_request_failed'
                        attempt['errorType'] = type(exc).__name__
                    finally:
                        if file_id is not None and api('/api/player/status').get('fileId') == file_id:
                            api('/api/player/stop', {})
                    if case['result'] == 'progress_verified':
                        break
                    save()
            else:
                case['result'] = 'case_timeout'
            case['elapsedSeconds'] = round(time.monotonic() - started, 1)
        except Exception as exc:
            case.update(result='request_failed', errorType=type(exc).__name__)
        finally:
            if stream is not None:
                stream.close()
            save()
            print('END', case.get('title', anime_id), case['result'], case.get('elapsedSeconds'), flush=True)
    report['finishedAt'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    save()
    expected = 'progress_verified' if args.play else 'candidate'
    return 0 if all(case['result'] == expected for case in report['cases']) else 1


if __name__ == '__main__':
    raise SystemExit(main())
