# Audio route recovery test

This procedure verifies that derpy recovers when a PulseAudio or PipeWire Bluetooth sink disappears.

## Before starting

Run these in separate terminals on Linux:

```sh
pactl subscribe
```

```sh
journalctl --user -f -u pipewire-pulse -u wireplumber
```

Start derpy with a directory containing at least one audio track:

```sh
derpy <music-directory>
```

While the track is playing, identify derpy's sink input:

```sh
pactl list sink-inputs
```

Find the entry whose application name is `derpy`, and note its `Sink:` and `State:` fields.

## Reproduction and recovery check

1. Pause playback with `SPACE`.
2. Disconnect or power off the Bluetooth headphones.
3. Confirm `pactl subscribe` reports the sink removal and that a replacement default sink exists:

   ```sh
   pactl get-default-sink
   pactl list short sinks
   ```

4. Resume playback with `SPACE`.
5. Confirm the track is audible through the current default sink.
6. Run `pactl list sink-inputs` again. The derpy sink input should exist and point at an available sink.
7. Confirm the derpy position does not advance while no output sink is available, then resumes after recovery.

## Useful failure evidence

Capture the following if recovery fails:

```sh
pactl list sink-inputs
pactl list short sinks
wpctl status
journalctl --user -u pipewire-pulse -u wireplumber --since "5 minutes ago"
```

A sink-input that is missing, corked, suspended, or still references a removed sink distinguishes an application recovery failure from a Bluetooth or PipeWire device-connection failure.
