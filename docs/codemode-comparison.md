# Codemode: four independent reads

With `OPENAI_API_KEY` set, run `npm run backend:bundle` and `node backend/bench/codemode.mjs`.
This makes real provider requests with a generated fixture, not source files from your checkout.
The optional argument is the number of paired runs (default 3).

Both variants use `openai/gpt-5.4-mini` with thinking `off`, the same Hush base prompt and working
directory, an empty starting thread, and the same prompt and four 81-line fixture files. The script
checks that both return the same four values. The enabled variant uses one Codemode call with four
nested reads in parallel and returns only the matching lines. The disabled variant uses four direct
reads in parallel; their full text goes to the model. The backend starts before the timer. Fresh
stores isolate the threads; the provider may still cache matching prompt prefixes.

One run on macOS arm64 (Node 22.23.5), medians over three pairs, with the order reversed in pair 2:

| Measure | Direct `read` | Codemode |
| --- | ---: | ---: |
| Model requests per run | 2 | 2 |
| Input tokens (including cache reads) | 5,484 | 2,624 |
| Output tokens | 103 | 146 |
| Total model tokens | 5,587 | 2,770 |
| Model request time | 1,724 ms | 2,121 ms |
| Tool wall time | 21 ms | 77 ms |
| End-to-end time | 1,767 ms | 2,213 ms |

Request time sums elapsed time from opening each provider stream to its final result. Tool time
uses the union of Durable's top-level tool intervals: parallel reads count once, and Codemode
includes worker startup, the nested reads, journaling and result formatting. End-to-end covers
submission through settlement. Cache reads were 0–4,480 tokens with direct reads and 1,152 in each
Codemode run; the token totals include them regardless of billing.

Codemode cut total tokens by about half, but did not save a round trip: direct tools already ran in
parallel. Tool time rose, and the median run was slower. Provider latency varied across pairs;
these samples do not establish a speedup or slowdown for other workloads.
