# cookie-clicker-bot

bot for https://stephenlb.github.io/cookie-clicker-but-multiplayer/

the site makes you do a little proof of work before it counts a click
(sha256 of nonce+uuid needs 5 leading zero hex chars) and then it just
publishes the result to a pubnub channel with the demo keys. so this
does that, but with 100 fake users at once.

## usage

```
go build
./cookie-clicker-bot                    # 100 users, clicks forever, routes through tor
./cookie-clicker-bot -tor off           # no tor, way faster
./cookie-clicker-bot -users 10 -clicks 5
```

ctrl+c to stop, it prints stats every 15s

flags:

- `-users` how many uuids to run (default 100)
- `-clicks` clicks per user, 0 = forever (default 0)
- `-tor` `auto` / `off` / socks5 addr (default auto)
- `-ua` user agent, defaults to some iphone safari one

## tor

`-tor auto` will use tor if you already have it running on 9050, or find
the tor browser's tor.exe, or just download the expert bundle from
torproject.org itself. each publish worker gets its own socks5
credentials so tor puts them on separate circuits, otherwise everything
piles onto one circuit and times out (found that out the hard way).

tor mode is slow, like 4-5 clicks/sec. direct is ~11/sec on my machine.
the bottleneck is the hashing anyway.

## notes

- pubnub demo keys are public and shared, the channel is `Cookies`, so
  everyone running this is clicking the same cookie. that's the point i
  guess
- the site verifies the pow client side so invalid hashes just get
  ignored, no point skipping the work
