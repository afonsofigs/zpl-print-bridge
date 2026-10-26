# zpl-print-bridge

Print ZPL shipping labels on a thermal printer that doesn't understand ZPL, on Linux.

Carriers and shipping portals often produce labels in ZPL, the language of Zebra printers. Many low-cost 4x6 thermal printers use TSPL. Sent raw ZPL, they print nothing useful. Some models also emulate ZPL, so check the manual first.

zpl-print-bridge acts as a network Zebra printer on port 9100. It renders each label on your computer and sends it to your printer as TSPL, or as a PDF through the printer's CUPS driver.

## How it works

```
app or print agent ─▶ 127.0.0.1:9100 ─▶ zpl-print-bridge
   (sends raw ZPL)                        ├─ renders with zebrash, at the printer resolution
                                          ├─ tspl mode: TSPL job ─▶ lp -o raw ─▶ CUPS queue ─▶ printer
                                          ├─ pdf mode:  PDF       ─▶ lp       ─▶ CUPS driver ─▶ printer
                                          └─ archive:   .zpl and .pdf of each job (optional)
```

The ZPL is rendered by [zebrash](https://github.com/ingridhq/zebrash), which runs locally, so label data stays on your computer. Each label is drawn dot for dot at the printer resolution, so barcode bars keep their exact width. Each job renders in a child process with a memory limit, so a malformed job can't stop the service.

## Requirements

- Linux with CUPS
- Go 1.25 or later, to build. If your distribution has an older Go, install a current one from [go.dev/dl](https://go.dev/dl/).
- `notify-send`, only for desktop notifications

## Install

```sh
git clone https://github.com/afonsofigs/zpl-print-bridge
cd zpl-print-bridge
GOBIN=$HOME/.local/bin go install .
```

### 1. Add a CUPS queue for the printer

In `tspl` mode the queue passes the job to the printer unchanged. Find the printer URI, then add a raw queue:

```sh
lpinfo -v | grep usb
lpadmin -p Label-Printer -E -v 'usb://...' -m raw -o printer-is-shared=false
```

Both commands need membership of the `lpadmin` group, or `sudo`. CUPS warns that raw queues and printer drivers are deprecated. This tool needs a raw queue in `tspl` mode.

Some distributions add a queue with a generic driver when a USB printer is plugged in. Remove that queue with `lpadmin -x <name>`, so no application prints to it by mistake.

In `pdf` mode, use a queue with the printer's own CUPS driver instead, and set the paper size in `lp_options`.

### 2. Configure

```sh
install -Dm644 zpl-print-bridge.conf.example ~/.config/zpl-print-bridge/config
```

Set `printer` to the queue name. Set the label size to the size of the labels you receive. Most carrier labels are 4x6 in. Some are 4x8 in.

The `dpi` setting works like this:

- In `tspl` mode it must match the printer and the label design. ZPL positions are in dots, so a 203 dpi label prints smaller on a 300 dpi printer.
- In `pdf` mode the page has the physical label size. The CUPS driver draws it at the printer resolution, so the label keeps its size. Barcodes are exact when the printer resolution equals `dpi`.

### 3. Run as a user service

```sh
install -Dm644 zpl-print-bridge.service ~/.config/systemd/user/zpl-print-bridge.service
systemctl --user daemon-reload
systemctl --user enable --now zpl-print-bridge
```

User services start when you log in. On a computer with no desktop login, run `loginctl enable-linger` once.

### 4. Send labels to it

- Applications that print to a network Zebra printer: use the address `127.0.0.1`, port `9100`.
- Applications and browser print agents that send raw commands to a CUPS printer: add a queue that forwards to the bridge. Select it as a Zebra printer.

```sh
lpadmin -p ZPL -E -v socket://127.0.0.1:9100 -m raw -o printer-is-shared=false
```

## Commands

Options such as `-config FILE` go before the command.

```sh
zpl-print-bridge                              # serve: listen and print
zpl-print-bridge print label.zpl              # print one file ("-" reads stdin)
zpl-print-bridge render label.zpl out.pdf     # preview as PDF
zpl-print-bridge render label.zpl out.tspl    # write the TSPL job
zpl-print-bridge -version
```

All commands read `~/.config/zpl-print-bridge/config`, or the file given with `-config`.

## Logs and troubleshooting

```sh
journalctl --user -u zpl-print-bridge -f
```

- `address already in use`: another program uses port 9100. Set another port in `listen`, and use it in the forwarding queue.
- The service stops with exit status 78: the configuration is invalid. The log names the setting.
- A label prints only after a delay: the sender keeps the connection open. The job ends after `read_timeout` without new data.
- If `lp` fails, the job is not printed. Set `archive_dir` to keep a copy of every job.

## Update

```sh
cd zpl-print-bridge
git pull
GOBIN=$HOME/.local/bin go install .
install -Dm644 zpl-print-bridge.service ~/.config/systemd/user/zpl-print-bridge.service
systemctl --user daemon-reload
systemctl --user restart zpl-print-bridge
```

A restart waits for the jobs in progress to print, for up to 25 minutes.

## Uninstall

```sh
systemctl --user disable --now zpl-print-bridge
rm ~/.local/bin/zpl-print-bridge ~/.config/systemd/user/zpl-print-bridge.service
systemctl --user daemon-reload
rm -r ~/.config/zpl-print-bridge
lpadmin -x ZPL
lpadmin -x Label-Printer
```

`lpadmin` needs the `lpadmin` group or `sudo`. Then:

- Delete the cloned `zpl-print-bridge` folder.
- If you set `archive_dir`, delete that folder too: it holds names and addresses.
- If you ran `loginctl enable-linger` only for this service, run `loginctl disable-linger`.
- If you don't use Go for anything else, run `go clean -cache -modcache` to free its caches, and delete `~/go`.

## Security

The bridge listens on `127.0.0.1` by default and has no authentication. It refuses HTTP requests, so web pages can't send labels to it.

If you set `listen` to a network address, every device that can reach the port can print. A wildcard address such as `0.0.0.0` also listens on IPv6. The bridge logs a warning at start. Allow only your network in the firewall. The bridge handles 4 connections at a time, and at most 2 from one IP address.

Malformed ZPL can make the renderer use a lot of memory or time. Each job renders in a child process limited to 512 MB, 1 minute for the first label, and at most 4 minutes in total. A job over a limit fails, and the service continues. The service process keeps itself below 384 MB, inside the 1 GB, no swap, that the systemd unit allows.

## Limitations

- ZPL support is what zebrash supports. Formats stored with `^DF` in one job and recalled with `^XF` in another job print blank.
- `^PQ` copies apply when each `^XA...^XZ` block that draws something gives one label.
- The bridge doesn't report printer status back to the sender. A label counts as printed when CUPS accepts it.

## Tested with

- Phomemo PM-246S-BT over USB, `tspl` mode, on Ubuntu 26.04, with labels from the CTT Portugal business portal.
- The 20 carrier labels in the zebrash test data, from Amazon, DHL, DPD, FedEx, GLS, UPS, USPS and others. They render dot for dot like the zebrash reference images.

## Related projects

- [zebrash](https://github.com/ingridhq/zebrash): the ZPL renderer used here.
- [virtual-zpl-printer](https://github.com/jochen42/virtual-zpl-printer) and [ZplPrinterPro](https://github.com/KaiJesussek/ZplPrinterPro): receive ZPL on port 9100 and show or store the labels, for testing.
- [tspl-cups-driver](https://github.com/RunTheWall/tspl-cups-driver): a CUPS driver that prints documents and images on TSPL printers.

## License

MIT. zebrash and its dependencies have their own licenses. zebrash embeds a font derived from Adobe Helvetica that has no redistribution license, so this project publishes no binary releases.
