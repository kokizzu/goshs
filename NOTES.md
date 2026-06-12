# TUI

There are some minor issues to the TUI which are:

- The TUI in general is just one line as header and one line for category and then a growing space of lines. It would look nicer to reserve a reasonable amount of space that will grow and incorporate a scrollbar, if running out of space maybe. You could reserve the space of the windows available.
- The Shell catcher will alert if any shell is connected. But it does not give you the ability to start/stop or interact with connected shells. So here the browser interface would be needed to be forwarded again
- SMTP looks good, but there is no possibility to save the attachements to disk to inspect them and also a html body does not get rendered while plaintext seams to work
- [DONE] DNS looks fine, except the time. It is 0001-01-01T00:00:00Z, that is wrong. → dnsserver now stamps Time: time.Now().
- [DONE] SMB looks sexy, but the hash will run out of line. It needs to be wrapped → detail view now hard-wraps to the window width.
- LDAP looks fine to me, no changes needed.
- [DONE] In HTTP collaborator in Web UI it pretty prints json and decrypt base64 for better usability. It does not in TUI. Lookup is normally in URL parameters, headers or body → tui/decode.go mirrors collab.js (JSON pretty-print, base64/JWT/form decode), applied to parameters, headers and body.
- [DONE] Collaborator events regardless which tab should be stored newest at the top. It is the other way around. → pane.add() now prepends.
- [DONE] For long running instances it would help to also have the date of the event and not just the time → summaries now use "01-02 15:04:05".
- I would love to have the name goshs be represented in a form of a banner at the top of the TUI somehow to make it look stylish and then move the information from the top to a real status bar at the bottom with the instructions on how to use (switch, scroll, detail, ...)
- In the status bar we could add more information like the version of goshs for example or the directory we are serving from. Maybe look at httpserver/info.go. Some of the information I would consider are: goshsversion, directory, ip/port/url, webdav, tunnel-url, cli enabled?, upload-folder if different from serve folder, auth user, process user, upload only, read only, no delete, shared links, dns port, smtp port, smb port and domain and share, ldap port, ttl-timer
- We could make more use of lipgloss to make it look a little nicer. Maybe incorporate nord theme again, as in the webUI and use emojis or icons for the tabs and the controls
- An export button for each tab of the collaborator to save the data and an overall export of all tabs like in the webUI would be great
- Hosting a clipboard tab would also be nice to interact with the clipboard from the TUI
- [DONE] While in details view, for example looking at http collab events, if a new event comes in the view updates. It would be better to stick to the details view regardless if there are any new → selection now anchors to the viewed event; new events only ride the top in the list view when already at the top.
  - After fixin the issue, now if a new record appears while in detail view, it will not be shown unless the users hits the up arrow. This is unfortunate. Is there a better way of doing it?

# WebUI

The WebUI is missing these additions:

- [DONE] reflect TTL somewhere if used. Maybe in the menu bar right above the version output? → live self-destruct countdown in the sidebar above the version button (meta ttl-deadline + inline ticker + .ttl-indicator styling).
