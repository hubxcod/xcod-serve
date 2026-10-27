# xcod

Live channel restreamer for Linux, with a web admin panel. It serves your channels as MPEG-TS or HLS, can transcode with ffmpeg, and scales across multiple servers.

A lightweight, self-hosted alternative to streaming servers such as Flussonic, Nimble Streamer or Wowza Streaming Engine: a single binary, no database, installed with one command.

## Install

On a Linux server (Ubuntu 22.04/24.04 or Debian 12), run as root:

```sh
curl -fsSL https://github.com/hubxcod/xcod-serve/releases/latest/download/install.sh | bash
```

When it finishes, it shows the panel address, the username and the password:

```
  Panel:     http://YOUR-IP:8090/G8rof035ZyFVzUkKEg5mQBBg/admin
  User:      admin
  Password:  ********
```

The panel lives at a secret address generated during installation; `/admin` alone answers 404. Save the address and the password: the password is shown only once.

> **Firewall:** open TCP ports **8090**, **80** and **443**. The installer opens them in `ufw`/`firewalld`, but if your hosting provider has its own firewall (cloud panel, security group), open them there too. Otherwise the panel and the streams will not be reachable.

To use a domain with HTTPS, add it later in the panel under **Server**, or pass it at install time:

```sh
curl -fsSL https://github.com/hubxcod/xcod-serve/releases/latest/download/install.sh | bash -s -- --domain video.example.com
```

## Usage

1. Open the panel and add a channel under **Channels**.
2. Watch it at `http://YOUR-IP:8090/live/CHANNEL.m3u8` (HLS) or `.../CHANNEL.ts` (MPEG-TS).
3. Add more servers under **Network**. The panel gives you a command to run on each new server.

Updates are installed from the panel, under **Server**.

## Contributing

Bug reports, ideas and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Uninstall

```sh
curl -fsSL https://github.com/hubxcod/xcod-serve/releases/latest/download/install.sh | bash -s -- --uninstall
```

## License

[AGPL-3.0](LICENSE). If you modify xcod and offer it as a service, you must publish your changes under the same license.
